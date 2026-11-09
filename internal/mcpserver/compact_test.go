package mcpserver

import (
	"context"
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
