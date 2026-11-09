package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// paramsTransport records the params of every call and answers like pane.read.
type paramsTransport struct{ params []string }

func (p *paramsTransport) Call(_ context.Context, _ string, params json.RawMessage) (json.RawMessage, error) {
	p.params = append(p.params, string(params))
	return json.RawMessage(`{"type":"agent_read","read":{"text":"hello"}}`), nil
}

func (p *paramsTransport) lastLines(t *testing.T) any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(p.params[len(p.params)-1]), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded["lines"]
}

func linesSchema(t *testing.T, session *mcp.ClientSession, tool string) map[string]any {
	t.Helper()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools.Tools {
		if candidate.Name != tool {
			continue
		}
		input, _ := candidate.InputSchema.(map[string]any)
		properties, _ := input["properties"].(map[string]any)
		lines, _ := properties["lines"].(map[string]any)
		return lines
	}
	t.Fatalf("tool %s not listed", tool)
	return nil
}

func allNotes(result *mcp.CallToolResult) string {
	var texts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func TestReadLinesDefaultAndCap(t *testing.T) {
	transport := &paramsTransport{}
	server := newAgentToolsServer(t, Options{Client: transport, ReadDefaultLines: 40, ReadMaxLines: 120})
	session := connectTestClient(t, server)

	for _, call := range []struct {
		name      string
		lines     any
		send      bool
		want      float64
		wantNoted bool
	}{
		{name: "omitted", want: 40},
		{name: "null", lines: nil, send: true, want: 40},
		{name: "under the cap", lines: 80, send: true, want: 80},
		{name: "at the cap", lines: 120, send: true, want: 120},
		{name: "over the cap", lines: 500, send: true, want: 120, wantNoted: true},
	} {
		arguments := map[string]any{"target": "reviewer", "source": "recent"}
		if call.send {
			arguments["lines"] = call.lines
		}
		result := callTestTool(t, session, "agent_read", arguments)
		if got := transport.lastLines(t); got != call.want {
			t.Fatalf("%s: lines sent to Herdr = %v, want %v", call.name, got, call.want)
		}
		noted := strings.Contains(allNotes(result), "Capped lines at 120 (requested 500)")
		if noted != call.wantNoted {
			t.Fatalf("%s: cap note present = %v, result = %s", call.name, noted, allNotes(result))
		}
	}

	lines := linesSchema(t, session, "agent_read")
	description, _ := lines["description"].(string)
	for _, want := range []string{"Defaults to 40", "At most 120"} {
		if !strings.Contains(description, want) {
			t.Fatalf("lines description %q lacks %q", description, want)
		}
	}
	if lines["default"] != float64(40) || lines["maximum"] != float64(120) {
		t.Fatalf("lines schema = %v", lines)
	}
	for _, method := range server.Methods {
		properties, _ := method.InputSchema["properties"].(map[string]any)
		if raw, _ := properties["lines"].(map[string]any); raw != nil && raw["description"] != nil {
			t.Fatalf("Methods carries the described %s schema; it should stay the raw Herdr one", method.Method)
		}
	}
}

// Without either flag, upstream behavior stands: an omitted lines stays
// omitted, a large one passes through, and the schema is Herdr's own.
func TestReadLinesUntouchedWithoutFlags(t *testing.T) {
	transport := &paramsTransport{}
	session := connectTestClient(t, newAgentToolsServer(t, Options{Client: transport}))

	callTestTool(t, session, "agent_read", map[string]any{"target": "reviewer", "source": "recent"})
	if got := transport.params[len(transport.params)-1]; got != `{"source":"recent","target":"reviewer"}` {
		t.Fatalf("params without flags = %s", got)
	}
	result := callTestTool(t, session, "agent_read", map[string]any{"target": "reviewer", "source": "recent", "lines": 5000})
	if got := transport.lastLines(t); got != float64(5000) {
		t.Fatalf("lines without flags = %v", got)
	}
	if len(result.Content) != 1 {
		t.Fatalf("unexpected notes without flags: %s", allNotes(result))
	}
	lines := linesSchema(t, session, "agent_read")
	for _, key := range []string{"description", "default", "maximum"} {
		if _, ok := lines[key]; ok {
			t.Fatalf("lines schema gained %q without flags: %v", key, lines)
		}
	}
}

func TestReadLinesOnlyApplyToReadMethods(t *testing.T) {
	server := &Server{readDefaultLines: 40, readMaxLines: 120}
	params := json.RawMessage(`{"pane_id":"w1:p1","lines":500}`)
	if got, note := server.limitReadLines("pane.wait_for_output", params); string(got) != string(params) || note != "" {
		t.Fatalf("pane.wait_for_output was limited: %s %q", got, note)
	}
	if got, note := server.limitReadLines("pane.read", params); !strings.Contains(string(got), `"lines":120`) || note == "" {
		t.Fatalf("pane.read was not capped: %s %q", got, note)
	}
	if got, _ := server.limitReadLines("agent.read", json.RawMessage(`{"lines":"many"}`)); string(got) != `{"lines":"many"}` {
		t.Fatalf("non-numeric lines was rewritten: %s", got)
	}
}

// A default above the cap would otherwise be capped and noted on every call
// that did not even ask for lines.
func TestReadDefaultLinesIsClampedToMax(t *testing.T) {
	transport := &paramsTransport{}
	session := connectTestClient(t, newAgentToolsServer(t, Options{Client: transport, ReadDefaultLines: 200, ReadMaxLines: 50}))
	result := callTestTool(t, session, "agent_read", map[string]any{"target": "reviewer", "source": "recent"})
	if got := transport.lastLines(t); got != float64(50) {
		t.Fatalf("lines = %v, want 50", got)
	}
	if len(result.Content) != 1 {
		t.Fatalf("an omitted lines produced a note: %s", allNotes(result))
	}
}

// A cap alone must also bound an omitted lines, or leaving lines out would
// return Herdr's own default (80 lines, measured on 2026-11-09) past a smaller
// cap. The schema then states the cap as the default too.
func TestReadMaxLinesAloneBoundsOmittedLines(t *testing.T) {
	transport := &paramsTransport{}
	session := connectTestClient(t, newAgentToolsServer(t, Options{Client: transport, ReadMaxLines: 50}))

	for _, call := range []struct {
		name  string
		lines any
		send  bool
		want  float64
	}{
		{name: "omitted", want: 50},
		{name: "null", lines: nil, send: true, want: 50},
		{name: "under the cap", lines: 20, send: true, want: 20},
	} {
		arguments := map[string]any{"target": "reviewer", "source": "recent"}
		if call.send {
			arguments["lines"] = call.lines
		}
		result := callTestTool(t, session, "agent_read", arguments)
		if got := transport.lastLines(t); got != call.want {
			t.Fatalf("%s: lines sent to Herdr = %v, want %v", call.name, got, call.want)
		}
		if len(result.Content) != 1 {
			t.Fatalf("%s: unexpected note: %s", call.name, allNotes(result))
		}
	}

	lines := linesSchema(t, session, "agent_read")
	if lines["default"] != float64(50) || lines["maximum"] != float64(50) {
		t.Fatalf("lines schema = %v", lines)
	}
	description, _ := lines["description"].(string)
	for _, want := range []string{"Defaults to 50", "At most 50"} {
		if !strings.Contains(description, want) {
			t.Fatalf("lines description %q lacks %q", description, want)
		}
	}
}

// A default without a cap fills in an omitted lines but leaves a large
// explicit request alone, and the schema states no maximum.
func TestReadDefaultLinesAloneDoesNotCap(t *testing.T) {
	transport := &paramsTransport{}
	session := connectTestClient(t, newAgentToolsServer(t, Options{Client: transport, ReadDefaultLines: 40}))
	callTestTool(t, session, "agent_read", map[string]any{"target": "reviewer", "source": "recent"})
	if got := transport.lastLines(t); got != float64(40) {
		t.Fatalf("omitted lines sent to Herdr = %v, want 40", got)
	}
	result := callTestTool(t, session, "agent_read", map[string]any{"target": "reviewer", "source": "recent", "lines": 5000})
	if got := transport.lastLines(t); got != float64(5000) {
		t.Fatalf("explicit lines sent to Herdr = %v, want 5000", got)
	}
	if len(result.Content) != 1 {
		t.Fatalf("unexpected note without a cap: %s", allNotes(result))
	}
	if _, ok := linesSchema(t, session, "agent_read")["maximum"]; ok {
		t.Fatal("lines schema gained a maximum without --read-max-lines")
	}
}
