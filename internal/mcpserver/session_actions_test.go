package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mrvnklm/herdr-mcp/internal/herdr"
)

const (
	sessionTestPane = "w1:p2"
	sessionTestUUID = "bf837653-33ed-45fc-9b05-a0591fd766f3"

	claudeInFront = `{"type":"pane_process_info","process_info":{"pane_id":"w1:p2","shell_pid":100,"foreground_process_group_id":200,"foreground_processes":[{"pid":200,"name":"2.1.319"},{"pid":201,"name":"node"}]}}`
	shellInFront  = `{"type":"pane_process_info","process_info":{"pane_id":"w1:p2","shell_pid":100,"foreground_process_group_id":100,"foreground_processes":[{"pid":100,"name":"zsh"}]}}`
	nullProcesses = `{"type":"pane_process_info","process_info":{"pane_id":"w1:p2","shell_pid":null,"foreground_process_group_id":null,"foreground_processes":[]}}`
)

func sessionAgentJSON(status string) string {
	return fmt.Sprintf(`{"type":"agent_info","agent":{"pane_id":"w1:p2","agent":"claude","agent_status":%q,"agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":%q}}}`, status, sessionTestUUID)
}

type fakeCall struct {
	method string
	params map[string]any
}

// fakeHerdrSocket answers on a real unix socket, so the tools run through the
// production client including its error decoding.
type fakeHerdrSocket struct {
	mu    sync.Mutex
	calls []fakeCall
	// agent is the agent.get result; empty answers agent_not_found.
	agent string
	// before and after answer pane.process_info around the /exit submission.
	before, after string
	exited        bool
	// hold, when set, blocks agent.get until closed.
	hold chan struct{}
}

func (f *fakeHerdrSocket) serve(t *testing.T) *herdr.Client {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "herdr.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				line, _ := bufio.NewReader(connection).ReadBytes('\n')
				var request struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				_ = json.Unmarshal(line, &request)
				_, _ = fmt.Fprintln(connection, f.answer(request.Method, request.Params))
			}()
		}
	}()
	return &herdr.Client{SocketPath: listener.Addr().String()}
}

func (f *fakeHerdrSocket) answer(method string, params map[string]any) string {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{method, params})
	hold := f.hold
	f.mu.Unlock()
	if method == "agent.get" && hold != nil {
		<-hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	result := `{"type":"ok"}`
	switch method {
	case "agent.get":
		if f.agent == "" {
			return `{"id":"t","error":{"code":"agent_not_found","message":"no agent"}}`
		}
		result = f.agent
	case "pane.process_info":
		result = f.before
		if f.exited {
			result = f.after
		}
	case "agent.prompt":
		if params["text"] == "/exit" {
			f.exited = true
		}
	}
	return fmt.Sprintf(`{"id":"t","result":%s}`, result)
}

func (f *fakeHerdrSocket) recorded() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// inputs returns every recorded call that writes to a terminal.
func (f *fakeHerdrSocket) inputs() []fakeCall {
	var writes []fakeCall
	for _, call := range f.recorded() {
		if call.method != "agent.get" && call.method != "pane.process_info" {
			writes = append(writes, call)
		}
	}
	return writes
}

func newSessionServer(t *testing.T, fake *fakeHerdrSocket) (*Server, *mcp.ClientSession) {
	t.Helper()
	server := newAgentToolsServer(t, Options{Client: fake.serve(t), SessionActions: true})
	server.Logf = func(string, ...any) {}
	server.sessionExitWait = 400 * time.Millisecond
	return server, connectTestClient(t, server)
}

func callSessionTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func sessionArgs() map[string]any { return map[string]any{"target": sessionTestPane} }

func TestSessionActionsRejectEverythingButAnIdleClaude(t *testing.T) {
	cases := []struct {
		name, agent, before, want string
	}{
		{"working", sessionAgentJSON("working"), claudeInFront, "is working"},
		{"blocked", sessionAgentJSON("blocked"), claudeInFront, "waiting for an answer"},
		{"unknown", sessionAgentJSON("unknown"), claudeInFront, "is unknown"},
		{"unknown pane", "", claudeInFront, "does not exist"},
		{"other agent", `{"agent":{"pane_id":"w1:p2","agent":"codex","agent_status":"idle"}}`, claudeInFront, "does not run Claude"},
		{"no agent", `{"agent":{"pane_id":"w1:p2","agent":null,"agent_status":"idle"}}`, claudeInFront, "does not run Claude"},
		{"other pane", `{"agent":{"pane_id":"w9:p9","agent":"claude","agent_status":"idle","agent_session":{"agent":"claude","kind":"id","value":"` + sessionTestUUID + `"}}}`, claudeInFront, "not a current pane id"},
		{"working other agent", `{"agent":{"pane_id":"w1:p2","agent":"codex","agent_status":"working"}}`, claudeInFront, "does not run Claude"},
		{"shell in front", sessionAgentJSON("idle"), shellInFront, "not in the foreground"},
		{"null process ids", sessionAgentJSON("idle"), nullProcesses, "did not report the foreground"},
	}
	for _, test := range cases {
		for _, action := range sessionActions {
			t.Run(test.name+"/"+action.tool, func(t *testing.T) {
				fake := &fakeHerdrSocket{agent: test.agent, before: test.before}
				_, session := newSessionServer(t, fake)
				result := callSessionTool(t, session, action.tool, sessionArgs())
				if !result.IsError || !strings.Contains(resultText(result), test.want) {
					t.Fatalf("result = %s, want an error containing %q", resultText(result), test.want)
				}
				if writes := fake.inputs(); len(writes) != 0 {
					t.Fatalf("rejected action wrote to the pane: %+v", writes)
				}
			})
		}
	}
}

func TestSessionRestartRejectsInvalidSessionIDs(t *testing.T) {
	template := `{"agent":{"pane_id":"w1:p2","agent":"claude","agent_status":"idle","agent_session":%s}}`
	session := func(agent, kind, value string) string {
		encoded, _ := json.Marshal(map[string]string{"agent": agent, "kind": kind, "value": value})
		return string(encoded)
	}
	for name, agentSession := range map[string]string{
		"null":           "null",
		"shell payload":  session("claude", "id", sessionTestUUID+"; rm -rf ~"),
		"newline":        session("claude", "id", sessionTestUUID+"\n"),
		"uppercase":      session("claude", "id", strings.ToUpper(sessionTestUUID)),
		"not a uuid":     session("claude", "id", "latest"),
		"other agent":    session("codex", "id", sessionTestUUID),
		"other ref kind": session("claude", "path", sessionTestUUID),
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeHerdrSocket{agent: fmt.Sprintf(template, agentSession), before: claudeInFront}
			_, client := newSessionServer(t, fake)
			result := callSessionTool(t, client, sessionRestartTool, sessionArgs())
			if !result.IsError || !strings.Contains(resultText(result), "no valid Claude session") {
				t.Fatalf("result = %s", resultText(result))
			}
			if writes := fake.inputs(); len(writes) != 0 {
				t.Fatalf("wrote to the pane: %+v", writes)
			}
		})
	}
}

func TestSessionActionsAcceptOnlyAPaneIDTarget(t *testing.T) {
	for name, arguments := range map[string]map[string]any{
		"agent name":    {"target": "reviewer"},
		"shell payload": {"target": "w1:p2; ls"},
		"empty":         {"target": ""},
		"missing":       {},
		"machine":       {"target": sessionTestPane, "machine": "ssh:other"},
		"free text":     {"target": sessionTestPane, "text": "hello"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeHerdrSocket{agent: sessionAgentJSON("idle"), before: claudeInFront}
			_, session := newSessionServer(t, fake)
			result := callSessionTool(t, session, "session_clear", arguments)
			if !result.IsError {
				t.Fatalf("accepted %v", arguments)
			}
			if calls := fake.recorded(); len(calls) != 0 {
				t.Fatalf("reached Herdr: %+v", calls)
			}
		})
	}
}

func TestSessionClearAndCompactSendFixedCommand(t *testing.T) {
	for tool, command := range map[string]string{"session_clear": "/clear", "session_compact": "/compact"} {
		for _, status := range []string{"idle", "done"} {
			t.Run(tool+"/"+status, func(t *testing.T) {
				fake := &fakeHerdrSocket{agent: sessionAgentJSON(status), before: claudeInFront}
				_, session := newSessionServer(t, fake)
				result := callSessionTool(t, session, tool, sessionArgs())
				if result.IsError {
					t.Fatal(resultText(result))
				}
				writes := fake.inputs()
				if len(writes) != 2 || writes[0].method != "pane.send_keys" || writes[1].method != "agent.prompt" {
					t.Fatalf("writes = %+v", writes)
				}
				if keys, _ := writes[0].params["keys"].([]any); len(keys) != sessionDraftClearKeys || keys[0] != "ctrl+u" {
					t.Fatalf("clear keys = %v", writes[0].params)
				}
				if writes[1].params["text"] != command || writes[1].params["target"] != sessionTestPane || writes[1].params["wait"] != nil {
					t.Fatalf("prompt params = %v", writes[1].params)
				}
			})
		}
	}
}

func TestSessionRestartResumesOnlyAfterTheShellIsBack(t *testing.T) {
	fake := &fakeHerdrSocket{agent: sessionAgentJSON("idle"), before: claudeInFront, after: shellInFront}
	_, session := newSessionServer(t, fake)
	result := callSessionTool(t, session, sessionRestartTool, sessionArgs())
	if result.IsError {
		t.Fatal(resultText(result))
	}
	writes := fake.inputs()
	if len(writes) != 3 || writes[1].params["text"] != "/exit" || writes[2].method != "pane.send_input" {
		t.Fatalf("writes = %+v", writes)
	}
	if want := "claude --resume " + sessionTestUUID; writes[2].params["text"] != want {
		t.Fatalf("resume text = %v, want %q", writes[2].params["text"], want)
	}
	if keys, _ := writes[2].params["keys"].([]any); len(keys) != 1 || keys[0] != "enter" {
		t.Fatalf("resume keys = %v", writes[2].params)
	}
	calls := fake.recorded()
	if last := calls[len(calls)-2]; last.method != "pane.process_info" {
		t.Fatalf("resume was not preceded by a process check: %+v", calls)
	}
}

func TestSessionRestartTypesNothingWithoutShellSignal(t *testing.T) {
	for name, after := range map[string]string{
		"claude stays in front": claudeInFront,
		"null ids":              nullProcesses,
		"zero ids":              `{"process_info":{"shell_pid":0,"foreground_process_group_id":0,"foreground_processes":[]}}`,
		"group equal, extra":    `{"process_info":{"shell_pid":100,"foreground_process_group_id":100,"foreground_processes":[{"pid":100},{"pid":300}]}}`,
		"garbage":               `{"process_info":"nope"}`,
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeHerdrSocket{agent: sessionAgentJSON("idle"), before: claudeInFront, after: after}
			_, session := newSessionServer(t, fake)
			result := callSessionTool(t, session, sessionRestartTool, sessionArgs())
			if !result.IsError || !strings.Contains(resultText(result), "not restarted") {
				t.Fatalf("result = %s", resultText(result))
			}
			for _, write := range fake.inputs() {
				if write.method == "pane.send_input" {
					t.Fatalf("typed into the pane: %+v", write)
				}
			}
		})
	}
}

// The app gives up after about 40 seconds and retries; a caller hanging up
// mid-restart must still get the session back.
func TestSessionRestartSurvivesCallerCancellation(t *testing.T) {
	fake := &fakeHerdrSocket{agent: sessionAgentJSON("idle"), before: claudeInFront, after: shellInFront}
	server, _ := newSessionServer(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := server.runSessionAction(ctx, sessionActions[2], json.RawMessage(`{"target":"w1:p2"}`))
	if result.IsError {
		t.Fatal(resultText(result))
	}
}

func TestSessionActionsCooldownSpansPanes(t *testing.T) {
	fake := &fakeHerdrSocket{agent: sessionAgentJSON("working"), before: claudeInFront}
	_, session := newSessionServer(t, fake)
	if result := callSessionTool(t, session, "session_clear", sessionArgs()); !strings.Contains(resultText(result), "is working") {
		t.Fatalf("result = %s", resultText(result))
	}

	// A rejection does not start the cooldown.
	fake.mu.Lock()
	fake.agent = sessionAgentJSON("idle")
	fake.mu.Unlock()
	if result := callSessionTool(t, session, "session_clear", sessionArgs()); result.IsError {
		t.Fatal(resultText(result))
	}
	before := len(fake.recorded())
	result := callSessionTool(t, session, "session_compact", map[string]any{"target": "w7:p1"})
	if !result.IsError || !strings.Contains(resultText(result), "Another session action") {
		t.Fatalf("result = %s", resultText(result))
	}
	if after := len(fake.recorded()); after != before {
		t.Fatalf("cooldown still reached Herdr: %d calls -> %d", before, after)
	}
}

func TestSessionActionsRunOneAtATime(t *testing.T) {
	fake := &fakeHerdrSocket{agent: sessionAgentJSON("idle"), before: claudeInFront, hold: make(chan struct{})}
	_, session := newSessionServer(t, fake)
	done := make(chan *mcp.CallToolResult, 1)
	go func() { done <- callSessionTool(t, session, "session_clear", sessionArgs()) }()
	for deadline := time.Now().Add(2 * time.Second); len(fake.recorded()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("first action never reached Herdr")
		}
	}
	second := callSessionTool(t, session, "session_clear", sessionArgs())
	if !second.IsError || !strings.Contains(resultText(second), "Another session action") {
		t.Fatalf("parallel call = %s", resultText(second))
	}
	close(fake.hold)
	if first := <-done; first.IsError {
		t.Fatal(resultText(first))
	}
	if writes := fake.inputs(); len(writes) != 2 {
		t.Fatalf("writes = %+v", writes)
	}
}

func listToolNames(t *testing.T, session *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]*mcp.Tool, len(tools.Tools))
	for _, tool := range tools.Tools {
		names[tool.Name] = tool
	}
	return names
}

func TestSessionActionsAbsentWithoutFlag(t *testing.T) {
	tools := listToolNames(t, connectTestClient(t, newAgentToolsServer(t, Options{})))
	for _, action := range sessionActions {
		if _, found := tools[action.tool]; found {
			t.Fatalf("%s is registered without --session-actions", action.tool)
		}
	}
}

func TestSessionActionsAreDeclaredDestructive(t *testing.T) {
	fake := &fakeHerdrSocket{}
	server, session := newSessionServer(t, fake)
	if _, _, err := server.Reload(server.Schema); err != nil {
		t.Fatal(err)
	}
	tools := listToolNames(t, session)
	for _, action := range sessionActions {
		tool, found := tools[action.tool]
		if !found {
			t.Fatalf("%s missing", action.tool)
		}
		annotation := tool.Annotations
		if annotation.ReadOnlyHint || annotation.IdempotentHint || annotation.DestructiveHint == nil || !*annotation.DestructiveHint ||
			annotation.OpenWorldHint == nil || *annotation.OpenWorldHint {
			t.Fatalf("%s annotations = %+v", action.tool, annotation)
		}
		input, _ := tool.InputSchema.(map[string]any)
		properties, _ := input["properties"].(map[string]any)
		if _, hasTarget := properties["target"]; !hasTarget || len(properties) != 1 {
			t.Fatalf("%s properties = %v", action.tool, properties)
		}
	}
}
