package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectTestClient serves server over in-memory transports and returns a
// connected client session.
func connectTestClient(t *testing.T, server *Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.MCP.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func callTestTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s failed: %s", name, resultText(result))
	}
	return result
}

// Without the flag the upstream behavior stands: structuredContent repeats the
// text. With it, a tool that has no outputSchema returns the text alone.
func TestCompactResultsDropsStructuredContentWithoutOutputSchema(t *testing.T) {
	schema, err := herdr.ParseSchema([]byte(bridgeSchema))
	if err != nil {
		t.Fatal(err)
	}
	for _, compact := range []bool{false, true} {
		server, err := New(Options{
			Schema:         schema,
			Client:         &recordingTransport{},
			Version:        "test",
			Machines:       &stubRouter{caller: &recordingTransport{}},
			CompactResults: compact,
		})
		if err != nil {
			t.Fatal(err)
		}
		session := connectTestClient(t, server)
		for _, call := range []struct {
			name      string
			arguments map[string]any
		}{
			{"pane_read", map[string]any{"pane_id": "p1"}},
			{machineListTool, map[string]any{}},
		} {
			result := callTestTool(t, session, call.name, call.arguments)
			if resultText(result) == "(no content)" {
				t.Fatalf("compact=%v %s lost its text content", compact, call.name)
			}
			if got := result.StructuredContent != nil; got == compact {
				t.Fatalf("compact=%v %s structuredContent present=%v", compact, call.name, got)
			}
		}
	}
}

// The spec requires structuredContent for a tool that declares an outputSchema,
// so the flag must leave those alone.
func TestCompactResultsKeepsStructuredContentWithOutputSchema(t *testing.T) {
	server := &Server{compactResults: true}
	withSchema := &mcp.Tool{Name: "typed", OutputSchema: map[string]any{"type": "object"}}
	result := server.trimStructured(withSchema, &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: `{"ok":true}`}},
		StructuredContent: map[string]any{"ok": true},
	})
	if result.StructuredContent == nil {
		t.Fatal("structuredContent dropped from a tool with an outputSchema")
	}
}

const agentToolsSchema = `{
  "protocol": 22,
  "schema_version": 1,
  "schemas": {
    "request": {
      "oneOf": [
        {"properties": {"method": {"const": "agent.list"}, "params": {"$ref": "#/schemas/request/$defs/AgentListParams"}}},
        {"properties": {"method": {"const": "agent.get"}, "params": {"$ref": "#/schemas/request/$defs/AgentTargetParams"}}},
        {"properties": {"method": {"const": "agent.read"}, "params": {"$ref": "#/schemas/request/$defs/AgentReadParams"}}}
      ],
      "$defs": {
        "AgentListParams": {"type": "object"},
        "AgentTargetParams": {
          "type": "object",
          "properties": {"target": {"type": "string"}},
          "required": ["target"]
        },
        "AgentReadParams": {
          "type": "object",
          "properties": {
            "target": {"type": "string"},
            "source": {"type": "string"},
            "lines": {"type": ["integer", "null"], "minimum": 0}
          },
          "required": ["target", "source"]
        }
      }
    }
  }
}`

// An agent.list result in Herdr's shape, pretty-printed so whitespace
// stripping is observable: one named, focused agent and one unnamed one.
const agentListFixture = `{
  "type": "agent_list",
  "agents": [
    {
      "terminal_id": "t1", "workspace_id": "w1", "tab_id": "w1:t1", "pane_id": "w1:p1",
      "name": "reviewer", "agent": "claude", "display_agent": "Claude",
      "agent_status": "working", "focused": true, "revision": 7,
      "cwd": "/Users/me/src/herdr-mcp", "foreground_cwd": "/Users/me/src/herdr-mcp/internal",
      "terminal_title": "\u001b]0;Fix tests\u0007", "terminal_title_stripped": "Fix tests",
      "tokens": {"context": "41%", "cost": "1.20"},
      "launch_pending": false, "interactive_ready": true, "state_labels": {}
    },
    {
      "terminal_id": "t2", "workspace_id": "w1", "tab_id": "w1:t1", "pane_id": "w1:p2",
      "name": null, "agent": "codex", "agent_status": "idle", "focused": false, "revision": 3,
      "cwd": "/srv/app/", "terminal_title_stripped": null, "tokens": {},
      "launch_pending": false, "interactive_ready": true
    }
  ]
}`

const agentGetFixture = `{
  "type": "agent_info",
  "agent": {"pane_id": "w1:p1", "name": "reviewer", "cwd": "/Users/me/src/herdr-mcp", "tokens": {"context": "41%"}}
}`

// methodTransport answers each method with a fixed result.
type methodTransport map[string]string

func (m methodTransport) Call(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(m[method]), nil
}

func newAgentToolsServer(t *testing.T, options Options) *Server {
	t.Helper()
	schema, err := herdr.ParseSchema([]byte(agentToolsSchema))
	if err != nil {
		t.Fatal(err)
	}
	options.Schema = schema
	options.Version = "test"
	if options.Client == nil {
		options.Client = methodTransport{"agent.list": agentListFixture, "agent.get": agentGetFixture}
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestCompactAgentListKeepsOnlySummaryFields(t *testing.T) {
	session := connectTestClient(t, newAgentToolsServer(t, Options{CompactResults: true}))
	text := resultText(callTestTool(t, session, "agent_list", map[string]any{}))
	want := `{"type":"agent_list","summary":{"idle":1,"working":1},"agents":[` +
		`{"pane_id":"w1:p1","name":"reviewer","status":"working","repo":"herdr-mcp","title":"Fix tests","context":"41%","focused":true},` +
		`{"pane_id":"w1:p2","status":"idle","repo":"app"}]}`
	if text != want {
		t.Fatalf("agent_list text =\n%s\nwant\n%s", text, want)
	}
}

// Without the flag agent_list, agent_get and their descriptions stay exactly
// what upstream returns.
func TestAgentResultsUnchangedWithoutCompactResults(t *testing.T) {
	server := newAgentToolsServer(t, Options{})
	session := connectTestClient(t, server)
	if text := resultText(callTestTool(t, session, "agent_list", map[string]any{})); text != agentListFixture {
		t.Fatalf("agent_list text changed without the flag:\n%s", text)
	}
	if text := resultText(callTestTool(t, session, "agent_get", map[string]any{"target": "reviewer"})); text != agentGetFixture {
		t.Fatalf("agent_get text changed without the flag:\n%s", text)
	}
	for _, method := range server.Methods {
		if got, want := server.describe(method.Method, method.InputSchema), toolDescription(method.Method, method.InputSchema); got != want {
			t.Fatalf("%s description changed without the flag: %q", method.Method, got)
		}
	}
}

// agent_get keeps every field under the flag and only loses whitespace.
func TestCompactAgentGetIsCompleteWithoutWhitespace(t *testing.T) {
	session := connectTestClient(t, newAgentToolsServer(t, Options{CompactResults: true}))
	text := resultText(callTestTool(t, session, "agent_get", map[string]any{"target": "reviewer"}))
	var want bytes.Buffer
	if err := json.Compact(&want, []byte(agentGetFixture)); err != nil {
		t.Fatal(err)
	}
	if text != want.String() {
		t.Fatalf("agent_get text = %s, want %s", text, want.String())
	}
}

func TestCompactDescriptionsPointFromAgentListToAgentGet(t *testing.T) {
	session := connectTestClient(t, newAgentToolsServer(t, Options{CompactResults: true}))
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptions := map[string]string{}
	for _, tool := range tools.Tools {
		descriptions[tool.Name] = tool.Description
	}
	for name, wants := range map[string][]string{
		"agent_list": {"compact summary", "agent_get"},
		"agent_get":  {"every field", "agent_list returns only a compact summary", "still REMAINING"},
	} {
		for _, want := range wants {
			if !strings.Contains(descriptions[name], want) {
				t.Fatalf("%s description %q lacks %q", name, descriptions[name], want)
			}
		}
	}
}

// A result that does not match Herdr's agent.list shape must not be emptied
// into a summary of nothing.
func TestCompactAgentListFallsBackOnUnexpectedShape(t *testing.T) {
	got := compactResult("agent.list", json.RawMessage(`{ "type": "agent_list", "items": [1] }`))
	if string(got) != `{"type":"agent_list","items":[1]}` {
		t.Fatalf("fallback = %s", got)
	}
	if got := compactResult("agent.list", json.RawMessage(`not json`)); string(got) != "not json" {
		t.Fatalf("invalid JSON was rewritten: %s", got)
	}
}
