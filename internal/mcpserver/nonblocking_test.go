package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mrvnklm/herdr-mcp/internal/herdr"
)

// busyTransport answers like Herdr for an agent in a configurable state:
// agent.wait fails with waitErr, agent.get reports status (or fails with
// getErr), and a scrollback read is refused unless source=visible.
type busyTransport struct {
	mu        sync.Mutex
	status    string
	getErr    error
	waitErr   error
	readErr   error
	launching bool
	calls     []string
	reads     []string
}

func newBusy(status string) *busyTransport {
	return &busyTransport{status: status, waitErr: &herdr.APIError{Code: "timeout", Message: "timed out waiting for agent status"}}
}

func (b *busyTransport) Call(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, method+" "+string(params))
	switch method {
	case "agent.wait":
		return nil, b.waitErr
	case "agent.list":
		return json.RawMessage(fmt.Sprintf(`{"type":"agent_list","agents":[{"pane_id":"w1:p1","name":"reviewer","launch_pending":%v}]}`, b.launching)), nil
	case "agent.get":
		if b.getErr != nil {
			return nil, b.getErr
		}
		return json.RawMessage(fmt.Sprintf(`{"type":"agent_info","agent":{"pane_id":"w1:p1","name":"reviewer","agent_status":%q,"launch_pending":%v,"interactive_ready":%v,"terminal_title_stripped":"Review","tokens":{"context":"41%%"}}}`, b.status, b.launching, !b.launching)), nil
	case "agent.read":
		b.reads = append(b.reads, string(params))
		if !strings.Contains(string(params), `"source":"visible"`) {
			return nil, &herdr.APIError{Code: "agent_not_idle", Message: "cannot read 80 lines while w1:p1 is working"}
		}
		if b.readErr != nil {
			return nil, b.readErr
		}
		return json.RawMessage(`{"type":"agent_read","text":"screen"}`), nil
	}
	return json.RawMessage(`{"type":"agent_list","agents":[]}`), nil
}

var agentArgsSchema = map[string]any{"type": "object", "properties": map[string]any{
	"target": map[string]any{"type": "string"}, "timeout_ms": map[string]any{}, "until": map[string]any{}, "source": map[string]any{}, "lines": map[string]any{},
}}

func TestExpiredWaitNoteFollowsStatus(t *testing.T) {
	for _, test := range []struct {
		name, status, until string
		want, avoid         string
	}{
		{name: "working", status: "working", want: "still working", avoid: "finished"},
		{name: "blocked", status: "blocked", want: "Tell the user now", avoid: "still working"},
		{name: "finished", status: "done", until: `["done","idle"]`, want: "Read its answer with agent_read now", avoid: "still working"},
		{name: "finished idle", status: "idle", want: "has finished", avoid: "not started"},
		{name: "not started after prompt", status: "idle", until: `["working","blocked"]`, want: "has not started", avoid: "finished"},
		{name: "unknown", status: "unknown", want: `status "unknown"`, avoid: "still working"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var log recorder
			server := newAgentToolsServer(t, Options{Client: newBusy(test.status), MaxWaitMS: 18000})
			server.Logf = log.logf
			arguments := `{"target":"w1:p1"}`
			if test.until != "" {
				arguments = `{"target":"w1:p1","until":` + test.until + `}`
			}
			result := server.dispatch(context.Background(), "agent.wait", agentArgsSchema, json.RawMessage(arguments))
			text := allNotes(result)
			if result.IsError {
				t.Fatalf("expired wait is an error: %s", text)
			}
			if !strings.Contains(text, `"timed_out":true`) || !strings.Contains(text, `"agent_status":"`+test.status+`"`) {
				t.Fatalf("result lacks the status: %s", text)
			}
			if !strings.Contains(text, test.want) || strings.Contains(text, test.avoid) {
				t.Fatalf("note does not fit status %s: %s", test.status, text)
			}
			if strings.Contains(text, "tokens") {
				t.Fatalf("status carries the full agent record: %s", text)
			}
			if !strings.Contains(log.joined(), "agent.wait expired") {
				t.Fatalf("expired wait not logged: %q", log.joined())
			}
		})
	}
}

func TestExpiredWaitWithoutStatusSaysSo(t *testing.T) {
	transport := newBusy("working")
	transport.getErr = &herdr.APIError{Code: "agent_not_found", Message: "gone"}
	server := newAgentToolsServer(t, Options{Client: transport, MaxWaitMS: 18000})
	result := server.dispatch(context.Background(), "agent.wait", agentArgsSchema, json.RawMessage(`{"target":"w1:p1"}`))
	text := allNotes(result)
	if result.IsError || !strings.Contains(text, `"status_error"`) || !strings.Contains(text, "could not be read") || strings.Contains(text, "still working") {
		t.Fatalf("result = %s", text)
	}
}

func TestOtherWaitErrorsStayErrors(t *testing.T) {
	transport := newBusy("working")
	transport.waitErr = &herdr.APIError{Code: "agent_not_found", Message: "no agent w9:p9"}
	server := newAgentToolsServer(t, Options{Client: transport, MaxWaitMS: 18000})
	result := server.dispatch(context.Background(), "agent.wait", agentArgsSchema, json.RawMessage(`{"target":"w9:p9"}`))
	if !result.IsError || !strings.Contains(allNotes(result), "agent_not_found") {
		t.Fatalf("result = %s", allNotes(result))
	}
}

func TestExpiredWaitStaysAnErrorWithoutCap(t *testing.T) {
	server := newAgentToolsServer(t, Options{Client: newBusy("working")})
	result := server.dispatch(context.Background(), "agent.wait", agentArgsSchema, json.RawMessage(`{"target":"w1:p1","timeout_ms":100}`))
	if !result.IsError {
		t.Fatalf("without --max-wait-ms the caller chose the timeout and must see it: %s", allNotes(result))
	}
}

// A launch that outlasts the cap ends with a status, not with an error and a
// second wait on top.
func TestLaunchingAgentReturnsStatusWithinOneCap(t *testing.T) {
	transport := newBusy("unknown")
	transport.launching = true
	server := newAgentToolsServer(t, Options{Client: transport, MaxWaitMS: 300})
	result := server.dispatch(context.Background(), "agent.wait", agentArgsSchema, json.RawMessage(`{"target":"w1:p1"}`))
	text := allNotes(result)
	if result.IsError || !strings.Contains(text, `"launching":true`) || !strings.Contains(text, "still launching") {
		t.Fatalf("result = %s", text)
	}
	for _, call := range transport.calls {
		if strings.HasPrefix(call, "agent.wait ") {
			t.Fatalf("waited again after the launch used the budget: %v", transport.calls)
		}
	}
}

func TestRemainingWaitShrinksTheBudget(t *testing.T) {
	got := remainingWait(json.RawMessage(`{"target":"a","timeout_ms":18000}`), 5000*1e6)
	if !strings.Contains(string(got), `"timeout_ms":13000`) {
		t.Fatalf("remainingWait = %s", got)
	}
	got = remainingWait(json.RawMessage(`{"target":"a","timeout_ms":100}`), 5000*1e6)
	if !strings.Contains(string(got), `"timeout_ms":1`) {
		t.Fatalf("exhausted budget = %s", got)
	}
}

func TestBusyReadFallsBackToVisible(t *testing.T) {
	var log recorder
	transport := newBusy("working")
	server := newAgentToolsServer(t, Options{Client: transport, MaxWaitMS: 18000})
	server.Logf = log.logf
	result := server.dispatch(context.Background(), "agent.read", agentArgsSchema, json.RawMessage(`{"target":"w1:p1","source":"recent_unwrapped","lines":80}`))
	if result.IsError {
		t.Fatalf("busy read is an error: %s", allNotes(result))
	}
	if len(transport.reads) != 2 || !strings.Contains(transport.reads[1], `"lines":80`) || !strings.Contains(transport.reads[1], `"target":"w1:p1"`) {
		t.Fatalf("reads = %v", transport.reads)
	}
	if !strings.Contains(allNotes(result), "source=visible") || !strings.Contains(log.joined(), "fell back to source=visible") {
		t.Fatalf("note or log missing: %s / %s", allNotes(result), log.joined())
	}
}

func TestFailedVisibleRetryReturnsTheOriginalError(t *testing.T) {
	transport := newBusy("working")
	transport.readErr = errors.New("socket closed")
	server := newAgentToolsServer(t, Options{Client: transport, MaxWaitMS: 18000})
	result := server.dispatch(context.Background(), "agent.read", agentArgsSchema, json.RawMessage(`{"target":"w1:p1","source":"recent","lines":80}`))
	if !result.IsError || !strings.Contains(allNotes(result), "agent_not_idle") {
		t.Fatalf("result = %s", allNotes(result))
	}
}

func TestBusyReadStaysAnErrorWithoutCap(t *testing.T) {
	transport := newBusy("working")
	server := newAgentToolsServer(t, Options{Client: transport})
	result := server.dispatch(context.Background(), "agent.read", agentArgsSchema, json.RawMessage(`{"target":"w1:p1","source":"recent","lines":80}`))
	if !result.IsError || len(transport.reads) != 1 {
		t.Fatalf("without --max-wait-ms the read must behave like upstream: isError=%v reads=%v", result.IsError, transport.reads)
	}
}

func TestPollingHintsAreGone(t *testing.T) {
	server := newAgentToolsServer(t, Options{Client: newBusy("working"), MaxWaitMS: 18000})
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
