package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/mrvnklm/herdr-mcp/internal/herdr"
)

// With no agents running the summary is an empty object and agents an empty
// array, never null, so a caller can iterate both without a nil check.
func TestCompactAgentListWithNoAgents(t *testing.T) {
	session := connectTestClient(t, newAgentToolsServer(t, Options{
		CompactResults: true,
		Client:         methodTransport{"agent.list": `{"type":"agent_list","agents":[]}`},
	}))
	text := resultText(callTestTool(t, session, "agent_list", map[string]any{}))
	if want := `{"type":"agent_list","summary":{},"agents":[]}`; text != want {
		t.Fatalf("agent_list text = %s, want %s", text, want)
	}
}

// Without the flag the agent tools keep upstream's structuredContent; with it
// they drop it. The first compact test only covers pane_read and machine_list.
func TestAgentToolsStructuredContentFollowsTheFlag(t *testing.T) {
	for _, compact := range []bool{false, true} {
		session := connectTestClient(t, newAgentToolsServer(t, Options{CompactResults: compact}))
		for name, arguments := range map[string]map[string]any{
			"agent_list": {},
			"agent_get":  {"target": "reviewer"},
		} {
			result := callTestTool(t, session, name, arguments)
			if got := result.StructuredContent != nil; got == compact {
				t.Fatalf("compact=%v %s structuredContent present=%v", compact, name, got)
			}
		}
	}
}

// Herdr answers lines=0 with an empty read (measured on 2026-11-09 against
// Herdr 0.9.1), so passing 0 through cannot bypass the cap; it must also not
// be replaced by the default or produce a note.
func TestReadLinesZeroPassesThroughUnchanged(t *testing.T) {
	transport := &paramsTransport{}
	session := connectTestClient(t, newAgentToolsServer(t, Options{Client: transport, ReadDefaultLines: 40, ReadMaxLines: 150}))
	result := callTestTool(t, session, "agent_read", map[string]any{"target": "reviewer", "source": "recent", "lines": 0})
	if got := transport.lastLines(t); got != float64(0) {
		t.Fatalf("lines sent to Herdr = %v, want 0", got)
	}
	if len(result.Content) != 1 {
		t.Fatalf("lines=0 produced a note: %s", allNotes(result))
	}
}

// Negative values, as a typo on the command line would give, disable the
// limits instead of capping every read at a negative count.
func TestNegativeReadLimitsAreIgnored(t *testing.T) {
	transport := &paramsTransport{}
	session := connectTestClient(t, newAgentToolsServer(t, Options{Client: transport, ReadDefaultLines: -1, ReadMaxLines: -5}))
	callTestTool(t, session, "agent_read", map[string]any{"target": "reviewer", "source": "recent", "lines": 500})
	if got := transport.lastLines(t); got != float64(500) {
		t.Fatalf("lines sent to Herdr = %v, want 500", got)
	}
	lines := linesSchema(t, session, "agent_read")
	for _, key := range []string{"description", "default", "maximum"} {
		if _, ok := lines[key]; ok {
			t.Fatalf("lines schema gained %q from negative limits: %v", key, lines)
		}
	}
}

// A schema reload re-registers every tool; the compact descriptions and the
// lines limits must survive it, or the first refresh silently undoes them.
func TestReloadKeepsCompactDescriptionsAndReadLimits(t *testing.T) {
	transport := &paramsTransport{}
	server := newAgentToolsServer(t, Options{Client: transport, CompactResults: true, ReadDefaultLines: 40, ReadMaxLines: 150})
	session := connectTestClient(t, server)
	schema, err := herdr.ParseSchema([]byte(agentToolsSchema))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.Reload(schema); err != nil {
		t.Fatal(err)
	}
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "agent_list" && !strings.Contains(tool.Description, "compact summary") {
			t.Fatalf("agent_list description after reload = %q", tool.Description)
		}
	}
	if lines := linesSchema(t, session, "agent_read"); lines["maximum"] != float64(150) || lines["default"] != float64(40) {
		t.Fatalf("lines schema after reload = %v", lines)
	}
	result := callTestTool(t, session, "agent_read", map[string]any{"target": "reviewer", "source": "recent", "lines": 500})
	if got := transport.lastLines(t); got != float64(150) || result.StructuredContent != nil {
		t.Fatalf("after reload lines = %v, structuredContent present = %v", got, result.StructuredContent != nil)
	}
}
