package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// waitTransport records agent.wait params and reports no launching agents.
type waitTransport struct{ waits []string }

func (w *waitTransport) Call(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if method == "agent.wait" {
		w.waits = append(w.waits, string(params))
	}
	return json.RawMessage(`{"type":"agent_list","agents":[]}`), nil
}

func callWait(t *testing.T, options Options, arguments map[string]any) (map[string]any, string) {
	t.Helper()
	transport := &waitTransport{}
	options.Client = transport
	server := newAgentToolsServer(t, options)
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	params, note := server.limitWait("agent.wait", encoded)
	if _, err := server.Client.Call(context.Background(), "agent.wait", params); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(transport.waits[0]), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded, note
}

func TestMaxWaitFillsAndCaps(t *testing.T) {
	const limit = 15000.0
	for _, test := range []struct {
		name      string
		timeout   any
		send      bool
		want      float64
		wantNoted bool
	}{
		{name: "omitted", want: limit},
		{name: "null", send: true, want: limit},
		{name: "zero", timeout: 0, send: true, want: limit},
		{name: "negative", timeout: -5, send: true, want: limit},
		{name: "fraction", timeout: 1.5, send: true, want: limit},
		{name: "text", timeout: "9000", send: true, want: limit},
		{name: "below", timeout: 2000, send: true, want: 2000},
		{name: "at the cap", timeout: 15000, send: true, want: limit},
		{name: "above", timeout: 900000, send: true, want: limit, wantNoted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := map[string]any{"target": "w1:p2"}
			if test.send {
				arguments["timeout_ms"] = test.timeout
			}
			decoded, note := callWait(t, Options{MaxWaitMS: int(limit)}, arguments)
			if decoded["timeout_ms"] != test.want {
				t.Fatalf("timeout_ms = %v, want %v", decoded["timeout_ms"], test.want)
			}
			if (note != "") != test.wantNoted || (test.wantNoted && !strings.Contains(note, "Capped timeout_ms")) {
				t.Fatalf("note = %q", note)
			}
		})
	}
}

func TestMaxWaitOffLeavesParamsAlone(t *testing.T) {
	server := newAgentToolsServer(t, Options{Client: &waitTransport{}})
	for _, params := range []string{`{"target":"a"}`, `{"target":"a","timeout_ms":900000}`, `{"target":"a","timeout_ms":0}`} {
		if got, note := server.limitWait("agent.wait", json.RawMessage(params)); string(got) != params || note != "" {
			t.Fatalf("limitWait(%s) = %s, %q", params, got, note)
		}
	}
}

func TestMaxWaitIgnoresOtherMethods(t *testing.T) {
	server := newAgentToolsServer(t, Options{Client: &waitTransport{}, MaxWaitMS: 100})
	params := `{"timeout_ms":900000}`
	if got, note := server.limitWait("pane.wait_for_output", json.RawMessage(params)); string(got) != params || note != "" {
		t.Fatalf("limitWait = %s, %q", got, note)
	}
}

// The cap must land before waitThroughLaunch, which reads timeout_ms itself.
func TestMaxWaitReachesSocketThroughDispatch(t *testing.T) {
	transport := &waitTransport{}
	server := newAgentToolsServer(t, Options{Client: transport, MaxWaitMS: 1234})
	input := map[string]any{"type": "object", "properties": map[string]any{
		"target": map[string]any{"type": "string"}, "timeout_ms": map[string]any{},
	}}
	result := server.dispatch(context.Background(), "agent.wait", input, json.RawMessage(`{"target":"w1:p2","timeout_ms":99999}`))
	if len(transport.waits) != 1 || !strings.Contains(transport.waits[0], `"timeout_ms":1234`) {
		t.Fatalf("waits = %v", transport.waits)
	}
	if !strings.Contains(allNotes(result), "Capped timeout_ms at 1234") {
		t.Fatalf("result = %s", allNotes(result))
	}
}

func TestMaxWaitDropsPromptWait(t *testing.T) {
	server := &Server{maxWaitMS: 18000}
	got, note := server.limitWait("agent.prompt", json.RawMessage(`{"target":"w1:p1","text":"hi","wait":{"timeout_ms":600000}}`))
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["wait"]; present || decoded["text"] != "hi" || decoded["target"] != "w1:p1" || note == "" {
		t.Fatalf("limitWait = %s, %q", got, note)
	}
	plain := `{"target":"w1:p1","text":"hi"}`
	if got, note := server.limitWait("agent.prompt", json.RawMessage(plain)); string(got) != plain || note != "" {
		t.Fatalf("no wait: limitWait = %s, %q", got, note)
	}
	off := &Server{}
	waiting := `{"target":"w1:p1","text":"hi","wait":{"timeout_ms":1}}`
	if got, note := off.limitWait("agent.prompt", json.RawMessage(waiting)); string(got) != waiting || note != "" {
		t.Fatalf("flag off: limitWait = %s, %q", got, note)
	}
}

// promptTransport answers agent.prompt with a fixed error, or nil for success.
type promptTransport struct{ err error }

func (p *promptTransport) Call(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
	if method == "agent.prompt" && p.err != nil {
		return nil, p.err
	}
	return json.RawMessage(`{"type":"agent_list","agents":[]}`), nil
}

func TestPromptWaitNoteOnlyAfterTheSend(t *testing.T) {
	const arguments = `{"target":"w1:p1","text":"hi","wait":{"timeout_ms":600000}}`
	input := map[string]any{"properties": map[string]any{"target": map[string]any{}, "text": map[string]any{}, "wait": map[string]any{}}}
	text := func(transportErr error) (bool, string) {
		server := newAgentToolsServer(t, Options{Client: &promptTransport{err: transportErr}, MaxWaitMS: 100})
		result := server.call(context.Background(), "agent.prompt", input, json.RawMessage(arguments))
		var all strings.Builder
		for _, content := range result.Content {
			if item, ok := content.(*mcp.TextContent); ok {
				all.WriteString(item.Text)
			}
		}
		return result.IsError, all.String()
	}
	if failed, got := text(errors.New("agent_blocked")); !failed || strings.Contains(got, "sent without waiting") {
		t.Fatalf("failed prompt: isError=%v, %q", failed, got)
	}
	if failed, got := text(nil); failed || !strings.Contains(got, "Prompt sent without waiting") || !strings.Contains(got, "Do not poll") {
		t.Fatalf("sent prompt: isError=%v, %q", failed, got)
	}
}

func TestPromptSchemaSaysWaitIsIgnored(t *testing.T) {
	input := map[string]any{"properties": map[string]any{"wait": map[string]any{"type": "object"}}}
	got := withPromptWaitIgnored(input)
	wait := got["properties"].(map[string]any)["wait"].(map[string]any)
	if !strings.Contains(wait["description"].(string), "Ignored") || wait["type"] != "object" {
		t.Fatalf("wait = %v", wait)
	}
	if _, touched := input["properties"].(map[string]any)["wait"].(map[string]any)["description"]; touched {
		t.Fatal("input schema was modified")
	}
}
