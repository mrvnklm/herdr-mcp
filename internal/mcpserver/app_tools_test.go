package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mrvnklm/herdr-mcp/internal/core"
)

// fakeCore answers hc commands from canned JSON per terminal and records
// every call.
type fakeCore struct {
	mu        sync.Mutex
	calls     [][]string
	list      string
	terminals map[string]string
	read      string
	wait      string
	sent      string
	sendError error
}

func (f *fakeCore) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	target := args[len(args)-1]
	switch args[0] {
	case "ls":
		return []byte(f.list), nil
	case "get":
		answer, found := f.terminals[target]
		if !found {
			return nil, &core.Error{Kind: "not_found", Message: fmt.Sprintf("no terminal %q", target)}
		}
		return []byte(answer), nil
	case "read":
		return []byte(f.read), nil
	case "wait":
		return []byte(f.wait), nil
	case "send":
		if f.sent == "" {
			return []byte(`{"status":"idle","started":true,"turn":4}`), f.sendError
		}
		return []byte(f.sent), f.sendError
	}
	return nil, errors.New("unexpected hc command")
}

func (f *fakeCore) recorded(command string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched [][]string
	for _, call := range f.calls {
		if call[0] == command {
			matched = append(matched, call)
		}
	}
	return matched
}

// appTerminal is an hc get answer; state "" means no agent status at all.
func appTerminal(address, state string) string {
	status := "null"
	if state != "" {
		status = fmt.Sprintf(`{"app":"claude-code","state":%q,"msg":"dialog open"}`, state)
	}
	return fmt.Sprintf(`{"address":%q,"cwd":"/Users/someone/project","state":"live","status":%s,"title":"Pricing","turn":3,"labels":{"where":"ideas / 1"},"pid":10,"foreground":{"pid":11,"name":"2.1.328","argv":["claude","--dangerously-skip-permissions"]}}`, address, status)
}

func newAppServer(t *testing.T, fake *fakeCore, options Options) *Server {
	t.Helper()
	options.Core = core.NewWithRunner(fake.run)
	server := newAgentToolsServer(t, options)
	server.Logf = func(string, ...any) {}
	return server
}

func TestAppToolsNeedTheirFlag(t *testing.T) {
	without := listToolNames(t, connectTestClient(t, newAgentToolsServer(t, Options{})))
	with := listToolNames(t, connectTestClient(t, newAppServer(t, &fakeCore{}, Options{})))
	for _, name := range []string{appListTool, appGetTool, appReadTool, appWaitTool, appPromptTool} {
		if _, found := without[name]; found {
			t.Fatalf("%s registered without --app", name)
		}
		tool, found := with[name]
		if !found {
			t.Fatalf("%s missing with --app", name)
		}
		input, _ := tool.InputSchema.(map[string]any)
		if input["additionalProperties"] != false {
			t.Fatalf("%s accepts unknown fields", name)
		}
	}
	if !with[appListTool].Annotations.ReadOnlyHint || with[appPromptTool].Annotations.ReadOnlyHint {
		t.Fatal("annotations do not match agent_list and agent_prompt")
	}
	if !strings.Contains(serverInstructions(Options{Core: core.NewWithRunner(nil)}), "app_list") {
		t.Fatal("instructions do not mention the app tools")
	}
}

// Only live agents are listed: a shell is nothing app_prompt may type into.
func TestAppListShowsOnlyLiveAgents(t *testing.T) {
	fake := &fakeCore{list: `{"terminals":[
		{"address":"t2","agent":"claude-code","cwd":"/b","state":"live","status":"done","turn":4,"where":"zeta / 1"},
		{"address":"t3","agent":"zsh","cwd":"/c","state":"live","turn":0,"where":"shell / 1"},
		{"address":"t4","agent":"claude-code","cwd":"/d","state":"finished","status":"done","turn":1,"where":"old / 1"},
		{"address":"t1","agent":"claude-code","cwd":"/a","state":"live","status":"working","turn":2,"where":"alpha / 1"}]}`}
	session := connectTestClient(t, newAppServer(t, fake, Options{}))
	text := resultText(callSessionTool(t, session, appListTool, map[string]any{}))
	if !strings.Contains(text, `"target":"t1"`) || !strings.Contains(text, `"target":"t2"`) || strings.Contains(text, `"t3"`) || strings.Contains(text, `"t4"`) {
		t.Fatalf("result = %s", text)
	}
	if strings.Index(text, `"t1"`) > strings.Index(text, `"t2"`) {
		t.Fatalf("not sorted by place: %s", text)
	}
}

func TestAppPromptSendsOnlyToAnAgentThatTakesInput(t *testing.T) {
	cases := []struct {
		state      string
		wantSend   []string
		wantResult string
	}{
		{"idle", []string{"send", "--json", "--if", "idle", "--", "t1", "hello"}, "Prompt sent"},
		{"done", []string{"send", "--json", "--", "t1", "hello"}, "Prompt sent"},
		{"working", nil, "still working"},
		{"blocked", nil, "waits for an answer on its screen (dialog open)"},
		{"error", nil, "is in status"},
		{"", nil, "runs no coding agent"},
	}
	for _, testCase := range cases {
		t.Run(testCase.state, func(t *testing.T) {
			fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", testCase.state)}}
			session := connectTestClient(t, newAppServer(t, fake, Options{}))
			result := callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "hello"})
			if text := allText(result); !strings.Contains(text, testCase.wantResult) {
				t.Fatalf("result = %s", text)
			}
			sends := fake.recorded("send")
			switch {
			case testCase.wantSend == nil && len(sends) != 0:
				t.Fatalf("sent anyway: %q", sends)
			case testCase.wantSend != nil && (len(sends) != 1 || !reflect.DeepEqual(sends[0], testCase.wantSend)):
				t.Fatalf("sends = %q", sends)
			}
			if (testCase.wantSend == nil) != result.IsError {
				t.Fatalf("IsError = %v", result.IsError)
			}
		})
	}
}

func TestAppPromptRefusesAFinishedTerminal(t *testing.T) {
	finished := strings.Replace(appTerminal("t1", "done"), `"state":"live"`, `"state":"finished"`, 1)
	fake := &fakeCore{terminals: map[string]string{"t1": finished}}
	session := connectTestClient(t, newAppServer(t, fake, Options{}))
	result := callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "hello"})
	if !result.IsError || len(fake.recorded("send")) != 0 {
		t.Fatalf("result = %s, sends = %q", resultText(result), fake.recorded("send"))
	}
}

// An escape sequence pasted into a terminal could steer the terminal itself;
// a bad target could reach a terminal nobody listed. Neither reaches hc.
func TestAppToolsRejectBadInputBeforeHC(t *testing.T) {
	fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", "idle")}}
	session := connectTestClient(t, newAppServer(t, fake, Options{}))
	for _, arguments := range []map[string]any{
		{"target": "t1", "text": "\x1b]0;owned\x07"},
		{"target": "t1", "text": "fine\rbut not"},
		{"target": "t1", "text": "   "},
		{"target": "t1", "text": strings.Repeat("a", appMaxPromptRunes+1)},
		{"target": "t1"},
		{"target": "vps/t1", "text": "hello"},
		{"target": "review", "text": "hello"},
		{"target": "t1", "text": "hello", "force": true},
		{"target": "t1", "text": "!rm -rf ~"},
		{"target": "t1", "text": "  /resume"},
		{"target": "t1", "text": "# remember this"},
	} {
		if result := callSessionTool(t, session, appPromptTool, arguments); !result.IsError {
			t.Fatalf("%v accepted: %s", arguments, resultText(result))
		}
	}
	for _, tool := range []string{appGetTool, appReadTool, appWaitTool} {
		if result := callSessionTool(t, session, tool, map[string]any{"target": "t1;id"}); !result.IsError {
			t.Fatalf("%s accepted a bad target", tool)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("reached hc: %q", fake.calls)
	}
}

// Line breaks and tabs are ordinary prompt text.
func TestAppPromptKeepsLineBreaks(t *testing.T) {
	fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", "idle")}}
	session := connectTestClient(t, newAppServer(t, fake, Options{}))
	text := "Erste Zeile\n\tzweite Zeile: Größe & Preis"
	if result := callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": text}); result.IsError {
		t.Fatal(resultText(result))
	}
	if sends := fake.recorded("send"); len(sends) != 1 || sends[0][len(sends[0])-1] != text {
		t.Fatalf("sends = %q", sends)
	}
}

func TestAppPromptSaysWhetherTextMayHaveArrived(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"refused": {&core.Error{Kind: "blocked", Message: "t1 is blocked"}, "Nothing was sent"},
		"unknown": {errors.New("hc send did not answer in time"), "may still have arrived"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", "idle")}, sendError: testCase.err}
			session := connectTestClient(t, newAppServer(t, fake, Options{}))
			result := callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "hello"})
			if !result.IsError || !strings.Contains(resultText(result), testCase.want) {
				t.Fatalf("result = %s", resultText(result))
			}
		})
	}
}

// A successful prompt leaves a log line, without the prompt itself.
func TestAppPromptLogsWithoutTheText(t *testing.T) {
	fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", "idle")}}
	server := newAppServer(t, fake, Options{})
	var lines []string
	server.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	session := connectTestClient(t, server)
	callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "geheimes Thema"})
	if len(lines) != 1 || !strings.Contains(lines[0], "t1") || strings.Contains(lines[0], "geheim") {
		t.Fatalf("log = %q", lines)
	}
}

func TestAppGetReportsWhatABlockedAgentWaitsFor(t *testing.T) {
	fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", "blocked"), "t2": appTerminal("t2", "")}}
	session := connectTestClient(t, newAppServer(t, fake, Options{}))
	text := resultText(callSessionTool(t, session, appGetTool, map[string]any{"target": "t1"}))
	for _, want := range []string{`"status":"blocked"`, `"waiting_for":"dialog open"`, `"where":"ideas / 1"`, `"title":"Pricing"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
	if result := callSessionTool(t, session, appGetTool, map[string]any{"target": "t2"}); !result.IsError {
		t.Fatalf("a shell was reported as a session: %s", resultText(result))
	}
	if result := callSessionTool(t, session, appGetTool, map[string]any{"target": "t9"}); !result.IsError || !strings.Contains(resultText(result), "app_list") {
		t.Fatalf("result = %s", resultText(result))
	}
}

func TestAppReadCapsHistory(t *testing.T) {
	fake := &fakeCore{read: `{"text":"screen","complete":false,"why":"working"}`}
	session := connectTestClient(t, newAppServer(t, fake, Options{ReadMaxLines: 120}))
	result := callSessionTool(t, session, appReadTool, map[string]any{"target": "t1", "lines": 5000})
	if result.IsError || !strings.Contains(allText(result), "is working") {
		t.Fatalf("result = %v", allText(result))
	}
	// As for agent_read, a cap without a default is the default.
	callSessionTool(t, session, appReadTool, map[string]any{"target": "t1"})
	plain := &fakeCore{read: `{"text":"screen","complete":true}`}
	callSessionTool(t, connectTestClient(t, newAppServer(t, plain, Options{})), appReadTool, map[string]any{"target": "t1"})
	reads := append(fake.recorded("read"), plain.recorded("read")...)
	if len(reads) != 3 || reads[0][3] != "120" || reads[1][3] != "120" || reads[2][3] != fmt.Sprint(appDefaultLines) {
		t.Fatalf("reads = %q", reads)
	}
}

func TestAppWaitReturnsTheStatusInsteadOfFailing(t *testing.T) {
	fake := &fakeCore{wait: `{"reason":"timeout","status":"working","ended":false,"turn":4}`}
	session := connectTestClient(t, newAppServer(t, fake, Options{MaxWaitMS: 2500}))
	result := callSessionTool(t, session, appWaitTool, map[string]any{"target": "t1"})
	if result.IsError || !strings.Contains(allText(result), "Do not call app_wait again") {
		t.Fatalf("result = %v", allText(result))
	}
	waits := fake.recorded("wait")
	if len(waits) != 1 || waits[0][3] != "2.500" {
		t.Fatalf("waits = %q", waits)
	}
}

func TestAppWaitNeverHoldsAClientLong(t *testing.T) {
	fake := &fakeCore{wait: `{"reason":"status","status":"done","ended":false,"turn":4}`}
	session := connectTestClient(t, newAppServer(t, fake, Options{MaxWaitMS: 600000}))
	result := callSessionTool(t, session, appWaitTool, map[string]any{"target": "t1"})
	if !strings.Contains(allText(result), "Read its answer with app_read") {
		t.Fatalf("result = %v", allText(result))
	}
	if waits := fake.recorded("wait"); waits[0][3] != "25.000" {
		t.Fatalf("waits = %q", waits)
	}
}

// allText joins every text content of a result, notes included.
func allText(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Measured 2026-11-19: hc reports ended true whenever the wait's condition
// was met, for a live agent too. Only an exit code or signal means the end.
func TestAppWaitTellsALiveAgentFromAnEndedOne(t *testing.T) {
	cases := map[string]string{
		`{"reason":"status","status":"blocked","ended":true,"exit_code":null,"signal":null,"turn":0}`: "blocked: it waits",
		`{"reason":"status","status":"idle","ended":true,"exit_code":null,"signal":null,"turn":2}`:    "Read its answer",
		`{"reason":"exit","status":null,"ended":true,"exit_code":0,"signal":null,"turn":2}`:           "has ended",
		`{"reason":"exit","status":null,"ended":true,"exit_code":null,"signal":1,"turn":2}`:           "has ended",
	}
	for answer, want := range cases {
		fake := &fakeCore{wait: answer}
		session := connectTestClient(t, newAppServer(t, fake, Options{}))
		result := callSessionTool(t, session, appWaitTool, map[string]any{"target": "t1"})
		if !strings.Contains(allText(result), want) {
			t.Fatalf("%s: result = %s", answer, allText(result))
		}
	}
}

// hc keeps "done" after Claude exits into a nested shell (hc docs: returning
// to a nested shell is not detected); the foreground program decides.
func TestAppPromptNeedsClaudeInTheForeground(t *testing.T) {
	shell := strings.Replace(appTerminal("t1", "done"), `"argv":["claude","--dangerously-skip-permissions"]`, `"argv":["bash"]`, 1)
	fake := &fakeCore{terminals: map[string]string{"t1": shell}}
	session := connectTestClient(t, newAppServer(t, fake, Options{}))
	result := callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "ls"})
	if !result.IsError || len(fake.recorded("send")) != 0 || !strings.Contains(resultText(result), "foreground") {
		t.Fatalf("result = %s, sends = %q", resultText(result), fake.recorded("send"))
	}
}

func TestAppPromptReportsWhatHCSaw(t *testing.T) {
	cases := map[string]string{
		`{"status":"idle","started":true,"turn":4}`:     "Prompt sent",
		`{"status":"done","started":null,"turn":4}`:     "Prompt sent",
		`{"status":"idle","started":false,"turn":4}`:    "did not start working",
		`{"status":null,"started":null,"turn":0}`:       "reported status unknown",
		`{"status":"blocked","started":false,"turn":4}`: "reported status blocked",
	}
	for answer, want := range cases {
		fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", "idle")}, sent: answer}
		session := connectTestClient(t, newAppServer(t, fake, Options{}))
		result := callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "hello"})
		if result.IsError || !strings.Contains(allText(result), want) {
			t.Fatalf("%s: result = %s", answer, allText(result))
		}
	}
}

// A timed-out send is probably still on its way; saying "may have arrived,
// check app_read" made a client resend before the first one showed.
func TestAppPromptTimeoutSaysDoNotResend(t *testing.T) {
	fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", "idle")}, sendError: fmt.Errorf("hc send did not answer in time: %w", context.DeadlineExceeded)}
	session := connectTestClient(t, newAppServer(t, fake, Options{}))
	result := callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "hello"})
	if !result.IsError || !strings.Contains(resultText(result), "Do not send it again") {
		t.Fatalf("result = %s", resultText(result))
	}
}

func TestAppPromptIsRateLimited(t *testing.T) {
	fake := &fakeCore{terminals: map[string]string{"t1": appTerminal("t1", "idle")}}
	server := newAppServer(t, fake, Options{})
	session := connectTestClient(t, server)
	for range appPromptsPerMinute {
		callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "hello"})
	}
	result := callSessionTool(t, session, appPromptTool, map[string]any{"target": "t1", "text": "hello"})
	if !result.IsError || !strings.Contains(resultText(result), "limit") || len(fake.recorded("send")) != appPromptsPerMinute {
		t.Fatalf("result = %s, sends = %d", resultText(result), len(fake.recorded("send")))
	}
	if err := server.appPromptAllowed(time.Now().Add(61 * time.Second)); err != nil {
		t.Fatalf("the limit did not expire: %v", err)
	}
}
