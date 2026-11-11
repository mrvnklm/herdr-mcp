package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrvnklm/herdr-mcp/internal/herdr"
)

// busyTransport answers like Herdr for an agent in the middle of a turn:
// agent.wait times out and a scrollback read is refused.
type busyTransport struct{ reads []string }

func (b *busyTransport) Call(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "agent.wait":
		return nil, &herdr.APIError{Code: "timeout", Message: "timed out waiting for agent status"}
	case "agent.get":
		return json.RawMessage(`{"type":"agent_info","agent":{"pane_id":"w1:p1","name":"reviewer","agent_status":"working","terminal_title_stripped":"Review","tokens":{"context":"41%"}}}`), nil
	case "agent.read":
		b.reads = append(b.reads, string(params))
		if !strings.Contains(string(params), `"source":"visible"`) {
			return nil, &herdr.APIError{Code: "agent_not_idle", Message: "cannot read 80 lines while w1:p1 is working"}
		}
		return json.RawMessage(`{"type":"agent_read","text":"screen"}`), nil
	}
	return json.RawMessage(`{"type":"agent_list","agents":[]}`), nil
}

var agentArgsSchema = map[string]any{"type": "object", "properties": map[string]any{
	"target": map[string]any{"type": "string"}, "timeout_ms": map[string]any{}, "source": map[string]any{}, "lines": map[string]any{},
}}

func TestExpiredWaitReturnsStatusNotError(t *testing.T) {
	server := newAgentToolsServer(t, Options{Client: &busyTransport{}, MaxWaitMS: 18000})
	result := server.dispatch(context.Background(), "agent.wait", agentArgsSchema, json.RawMessage(`{"target":"w1:p1"}`))
	if result.IsError {
		t.Fatalf("expired wait is an error: %s", allNotes(result))
	}
	text := allNotes(result)
	for _, want := range []string{`"timed_out":true`, `"agent_status":"working"`, "Do not call agent_wait again"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "tokens") {
		t.Fatalf("status carries the full agent record: %s", text)
	}
}

func TestExpiredWaitStaysAnErrorWithoutCap(t *testing.T) {
	server := newAgentToolsServer(t, Options{Client: &busyTransport{}})
	result := server.dispatch(context.Background(), "agent.wait", agentArgsSchema, json.RawMessage(`{"target":"w1:p1","timeout_ms":100}`))
	if !result.IsError {
		t.Fatalf("without --max-wait-ms the caller chose the timeout and must see it: %s", allNotes(result))
	}
}

func TestBusyReadFallsBackToVisible(t *testing.T) {
	transport := &busyTransport{}
	server := newAgentToolsServer(t, Options{Client: transport, MaxWaitMS: 18000})
	result := server.dispatch(context.Background(), "agent.read", agentArgsSchema, json.RawMessage(`{"target":"w1:p1","source":"recent_unwrapped","lines":80}`))
	if result.IsError {
		t.Fatalf("busy read is an error: %s", allNotes(result))
	}
	if len(transport.reads) != 2 || !strings.Contains(transport.reads[1], `"lines":80`) {
		t.Fatalf("reads = %v", transport.reads)
	}
	if !strings.Contains(allNotes(result), "source=visible") {
		t.Fatalf("no fallback note: %s", allNotes(result))
	}
}

func TestBusyReadStaysAnErrorWithoutCap(t *testing.T) {
	transport := &busyTransport{}
	server := newAgentToolsServer(t, Options{Client: transport})
	result := server.dispatch(context.Background(), "agent.read", agentArgsSchema, json.RawMessage(`{"target":"w1:p1","source":"recent","lines":80}`))
	if !result.IsError || len(transport.reads) != 1 {
		t.Fatalf("without --max-wait-ms the read must behave like upstream: isError=%v reads=%v", result.IsError, transport.reads)
	}
}

func TestPollingHintsAreGone(t *testing.T) {
	server := newAgentToolsServer(t, Options{Client: &busyTransport{}, MaxWaitMS: 18000})
	_, promptNote := server.limitWait("agent.prompt", json.RawMessage(`{"target":"a","text":"x","wait":{}}`))
	_, capNote := server.limitWait("agent.wait", json.RawMessage(`{"target":"a","timeout_ms":900000}`))
	description := server.describe("agent.wait", agentArgsSchema)
	for _, text := range []string{promptNote, capNote, description} {
		if strings.Contains(text, "agent_wait again to keep") || strings.Contains(text, "then agent_wait again") {
			t.Fatalf("polling hint left: %q", text)
		}
	}
	if !strings.Contains(description, "do not call this in a loop") {
		t.Fatalf("description = %q", description)
	}
	if !strings.Contains(serverInstructions(Options{MaxWaitMS: 1}), "Never poll") || strings.Contains(serverInstructions(Options{}), "Never poll") {
		t.Fatal("instructions do not follow --max-wait-ms")
	}
}
