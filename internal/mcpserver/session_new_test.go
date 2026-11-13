package mcpserver

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	sessionNewTestCWD  = "/Users/someone/project"
	sessionNewTestPane = "w1:p9"
	sessionNewTestTab  = "w1:t4"
)

var sessionNewTestArgs = []string{"--dangerously-skip-permissions"}

// sessionNewAgentJSON describes the source session; cwd is raw JSON so a test
// can pass null.
func sessionNewAgentJSON(agent, status, cwd string) string {
	return fmt.Sprintf(`{"type":"agent_info","agent":{"pane_id":"w1:p2","agent":%q,"agent_status":%q,"workspace_id":"w1","tab_id":"w1:t1","cwd":%s}}`, agent, status, cwd)
}

func sourceAgent() string {
	return sessionNewAgentJSON("claude", "idle", fmt.Sprintf("%q", sessionNewTestCWD))
}

// newSessionAgentJSON is the agent.get answer for the new pane.
func newSessionAgentJSON(status string, launchPending bool) string {
	return fmt.Sprintf(`{"type":"agent_info","agent":{"pane_id":"w1:p9","agent":"claude","agent_status":%q,"launch_pending":%t,"workspace_id":"w1","tab_id":"w1:t4"}}`, status, launchPending)
}

// newPaneShell and newPaneClaude answer pane.process_info for the new pane.
var (
	newPaneShell  = strings.ReplaceAll(shellInFront, "w1:p2", "w1:p9")
	newPaneClaude = strings.ReplaceAll(claudeInFront, "w1:p2", "w1:p9")
)

// healthyFake is a source Claude in front and a new pane whose shell settles,
// then runs Claude, which ends up idle.
func healthyFake() *fakeHerdrSocket {
	return &fakeHerdrSocket{
		agent:        sourceAgent(),
		before:       claudeInFront,
		newProcesses: []string{newPaneShell, newPaneClaude},
		newAgent:     newSessionAgentJSON("idle", false),
	}
}

// freshNewPane rewinds the new pane's process sequence for another start.
func freshNewPane(fake *fakeHerdrSocket) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.newProcesses = []string{newPaneShell, newPaneClaude}
}

func newSessionNewServer(t *testing.T, fake *fakeHerdrSocket) *Server {
	t.Helper()
	server := newAgentToolsServer(t, Options{
		Client:         fake.serve(t),
		SessionActions: true,
		SessionNew:     true,
		SessionNewArgs: sessionNewTestArgs,
	})
	server.Logf = func(string, ...any) {}
	server.sessionNewSettle = 600 * time.Millisecond
	server.sessionNewReadyWait = 900 * time.Millisecond
	server.sessionNewGoneAfter = 300 * time.Millisecond
	return server
}

func callsTo(fake *fakeHerdrSocket, method string) []fakeCall {
	var matched []fakeCall
	for _, call := range fake.recorded() {
		if call.method == method {
			matched = append(matched, call)
		}
	}
	return matched
}

func assertTabClosed(t *testing.T, fake *fakeHerdrSocket) {
	t.Helper()
	closes := callsTo(fake, "tab.close")
	if len(closes) != 1 || closes[0].params["tab_id"] != sessionNewTestTab {
		t.Fatalf("tab.close calls = %+v", closes)
	}
}

func assertTabKept(t *testing.T, fake *fakeHerdrSocket) {
	t.Helper()
	if closes := callsTo(fake, "tab.close"); len(closes) != 0 {
		t.Fatalf("tab.close calls = %+v", closes)
	}
}

// The source may be busy: a new tab leaves its pane alone.
func TestSessionNewStartsClaudeBesideTheSource(t *testing.T) {
	fake := healthyFake()
	fake.agent = sessionNewAgentJSON("claude", "working", fmt.Sprintf("%q", sessionNewTestCWD))
	session := connectTestClient(t, newSessionNewServer(t, fake))
	result := callSessionTool(t, session, sessionNewTool, sessionArgs())
	if result.IsError || !strings.Contains(resultText(result), "agent_prompt and target "+sessionNewTestPane) {
		t.Fatalf("result = %s", resultText(result))
	}

	tabs := callsTo(fake, "tab.create")
	if len(tabs) != 1 {
		t.Fatalf("tab.create calls = %+v", tabs)
	}
	if params := tabs[0].params; params["workspace_id"] != "w1" || params["cwd"] != sessionNewTestCWD || params["focus"] != false {
		t.Fatalf("tab.create params = %v", params)
	}

	starts := callsTo(fake, "agent.start")
	if len(starts) != 1 {
		t.Fatalf("agent.start calls = %+v", starts)
	}
	params := starts[0].params
	if params["kind"] != "claude" || params["pane_id"] != sessionNewTestPane || params["name"] != "session-9" {
		t.Fatalf("agent.start params = %v", params)
	}
	if args, _ := params["args"].([]any); !reflect.DeepEqual(args, []any{"--dangerously-skip-permissions"}) {
		t.Fatalf("agent.start args = %#v", params["args"])
	}
	for _, call := range fake.recorded() {
		if call.method == "agent.prompt" || call.method == "pane.send_keys" || call.method == "pane.send_input" {
			t.Fatalf("session_new wrote to a terminal: %+v", call)
		}
	}
	assertTabKept(t, fake)
}

func TestSessionNewRejectsAnUnsuitableSource(t *testing.T) {
	quoted := fmt.Sprintf("%q", sessionNewTestCWD)
	cases := []struct {
		name, agent, processes, want string
	}{
		{"no agent", "", claudeInFront, "does not exist"},
		{"not claude", sessionNewAgentJSON("codex", "idle", quoted), claudeInFront, "does not run Claude"},
		{"no directory", sessionNewAgentJSON("claude", "idle", "null"), claudeInFront, "usable directory"},
		{"relative directory", sessionNewAgentJSON("claude", "idle", `"project"`), claudeInFront, "usable directory"},
		{"unclean directory", sessionNewAgentJSON("claude", "idle", `"/Users/someone/../root"`), claudeInFront, "usable directory"},
		{"stale pane id", strings.Replace(sourceAgent(), `"pane_id":"w1:p2"`, `"pane_id":"w1:p7"`, 1), claudeInFront, "not a current pane id"},
		{"no workspace", strings.Replace(sourceAgent(), `"workspace_id":"w1"`, `"workspace_id":""`, 1), claudeInFront, "workspace"},
		// A stale status: Claude has ended and the shell may have moved on.
		{"shell in front", sourceAgent(), shellInFront, "not in the foreground"},
		{"unknown foreground", sourceAgent(), nullProcesses, "not in the foreground"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := healthyFake()
			fake.agent, fake.before = testCase.agent, testCase.processes
			session := connectTestClient(t, newSessionNewServer(t, fake))
			result := callSessionTool(t, session, sessionNewTool, sessionArgs())
			if !result.IsError || !strings.Contains(resultText(result), testCase.want) {
				t.Fatalf("result = %s", resultText(result))
			}
			if tabs := callsTo(fake, "tab.create"); len(tabs) != 0 {
				t.Fatalf("a rejected source still opened a tab: %+v", tabs)
			}
		})
	}
}

func TestSessionNewAcceptsOnlyAPaneIDTarget(t *testing.T) {
	fake := healthyFake()
	session := connectTestClient(t, newSessionNewServer(t, fake))
	for _, arguments := range []map[string]any{
		{},
		{"target": "my-agent"},
		{"target": sessionTestPane, "cwd": "/"},
	} {
		if result := callSessionTool(t, session, sessionNewTool, arguments); !result.IsError {
			t.Fatalf("%v accepted: %s", arguments, resultText(result))
		}
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("invalid input reached Herdr: %+v", calls)
	}
}

func TestSessionNewSharesTheCooldownBothWays(t *testing.T) {
	fake := healthyFake()
	session := connectTestClient(t, newSessionNewServer(t, fake))
	if result := callSessionTool(t, session, "session_clear", sessionArgs()); result.IsError {
		t.Fatal(resultText(result))
	}
	result := callSessionTool(t, session, sessionNewTool, sessionArgs())
	if text := resultText(result); !result.IsError || !strings.Contains(text, "Another session action") || !strings.Contains(text, "instead of retrying") {
		t.Fatalf("result = %s", text)
	}
	if tabs := callsTo(fake, "tab.create"); len(tabs) != 0 {
		t.Fatalf("cooldown still opened a tab: %+v", tabs)
	}

	fake = healthyFake()
	session = connectTestClient(t, newSessionNewServer(t, fake))
	if result := callSessionTool(t, session, sessionNewTool, sessionArgs()); result.IsError {
		t.Fatal(resultText(result))
	}
	result = callSessionTool(t, session, "session_clear", sessionArgs())
	if !result.IsError || !strings.Contains(resultText(result), "Another session action") {
		t.Fatalf("result = %s", resultText(result))
	}
}

func TestSessionNewStopsAtTheHourlyLimit(t *testing.T) {
	fake := healthyFake()
	server := newSessionNewServer(t, fake)
	server.sessionCooldown = 0
	session := connectTestClient(t, server)
	for attempt := 0; attempt < defaultSessionNewPerHour; attempt++ {
		freshNewPane(fake)
		if result := callSessionTool(t, session, sessionNewTool, sessionArgs()); result.IsError {
			t.Fatalf("start %d: %s", attempt+1, resultText(result))
		}
	}
	result := callSessionTool(t, session, sessionNewTool, sessionArgs())
	if !result.IsError || !strings.Contains(resultText(result), "limit") {
		t.Fatalf("result = %s", resultText(result))
	}
	if tabs := callsTo(fake, "tab.create"); len(tabs) != defaultSessionNewPerHour {
		t.Fatalf("tab.create calls = %d", len(tabs))
	}

	// The window slides: once the oldest start is an hour old, one more fits.
	server.sessionMu.Lock()
	server.sessionNewStarts[0] = time.Now().Add(-sessionNewWindow - time.Second)
	server.sessionMu.Unlock()
	freshNewPane(fake)
	if result := callSessionTool(t, session, sessionNewTool, sessionArgs()); result.IsError {
		t.Fatal(resultText(result))
	}
}

// A failed start whose tab was closed again still used up a slot.
func TestSessionNewCountsAFailedAttempt(t *testing.T) {
	fake := healthyFake()
	fake.newProcesses = []string{newPaneShell}
	fake.startError = "agent_name_taken"
	server := newSessionNewServer(t, fake)
	server.sessionCooldown = 0
	session := connectTestClient(t, server)
	if result := callSessionTool(t, session, sessionNewTool, sessionArgs()); !result.IsError {
		t.Fatal(resultText(result))
	}
	server.sessionMu.Lock()
	count, last := len(server.sessionNewStarts), server.lastSessionAction
	server.sessionMu.Unlock()
	if count != 1 || last.IsZero() {
		t.Fatalf("starts = %d, last action = %v", count, last)
	}
}

func TestSessionNewClosesTheTabWhenHerdrRefusesTheStart(t *testing.T) {
	fake := healthyFake()
	fake.newProcesses = []string{newPaneShell}
	fake.startError = "agent_name_taken"
	session := connectTestClient(t, newSessionNewServer(t, fake))
	result := callSessionTool(t, session, sessionNewTool, sessionArgs())
	if text := resultText(result); !result.IsError || !strings.Contains(text, "closed again") || !strings.Contains(text, "instead of retrying") {
		t.Fatalf("result = %s", text)
	}
	assertTabClosed(t, fake)
}

// Claude in front means it may still come up: closing the tab would kill it.
func TestSessionNewKeepsATabWhereClaudeRuns(t *testing.T) {
	fake := healthyFake()
	fake.startError = "agent_pane_busy"
	session := connectTestClient(t, newSessionNewServer(t, fake))
	result := callSessionTool(t, session, sessionNewTool, sessionArgs())
	if !result.IsError || !strings.Contains(resultText(result), "may still be starting in "+sessionNewTestPane) {
		t.Fatalf("result = %s", resultText(result))
	}
	assertTabKept(t, fake)
}

// Claude that exits at once (bad arguments, a crash) must not be reported as
// launched, and its tab must not stay behind with a bare shell.
func TestSessionNewClosesTheTabWhenClaudeExits(t *testing.T) {
	for _, startError := range []string{"", "agent_not_ready"} {
		fake := healthyFake()
		fake.newProcesses = []string{newPaneShell, newPaneClaude, newPaneShell}
		fake.newAgent = ""
		fake.startError = startError
		session := connectTestClient(t, newSessionNewServer(t, fake))
		result := callSessionTool(t, session, sessionNewTool, sessionArgs())
		if text := resultText(result); !result.IsError || !strings.Contains(text, "exited right after") {
			t.Fatalf("start error %q: result = %s", startError, text)
		}
		assertTabClosed(t, fake)
	}
}

// A trust or login prompt is left for the user, with nothing typed into it,
// whether Herdr reports it from agent.start or only later.
func TestSessionNewLeavesAStartupQuestionToTheUser(t *testing.T) {
	for _, startError := range []string{"", "agent_not_ready"} {
		fake := healthyFake()
		fake.newAgent = newSessionAgentJSON("blocked", true)
		fake.startError = startError
		session := connectTestClient(t, newSessionNewServer(t, fake))
		result := callSessionTool(t, session, sessionNewTool, sessionArgs())
		if result.IsError || !strings.Contains(resultText(result), "Tell the user") {
			t.Fatalf("start error %q: result = %s", startError, resultText(result))
		}
		for _, call := range fake.recorded() {
			if call.method == "tab.close" || call.method == "pane.send_keys" || call.method == "agent.prompt" {
				t.Fatalf("unexpected call %+v", call)
			}
		}
	}
}

func TestSessionNewNeverCallsAPendingLaunchIdle(t *testing.T) {
	for _, agent := range []string{newSessionAgentJSON("idle", true), newSessionAgentJSON("unknown", true), ""} {
		fake := healthyFake()
		fake.newAgent = agent
		session := connectTestClient(t, newSessionNewServer(t, fake))
		result := callSessionTool(t, session, sessionNewTool, sessionArgs())
		if text := resultText(result); result.IsError || strings.Contains(text, "It is idle") || !strings.Contains(text, "not ready yet") {
			t.Fatalf("new agent %q: result = %s", agent, text)
		}
		assertTabKept(t, fake)
	}
}

// Measured on a fresh tab: zsh runs its startup files in the foreground
// first, and Herdr refuses agent.start until the shell is back in front.
func TestSessionNewWaitsForTheFreshShell(t *testing.T) {
	fake := healthyFake()
	fake.newProcesses = []string{newPaneClaude, newPaneClaude, newPaneShell, newPaneClaude}
	session := connectTestClient(t, newSessionNewServer(t, fake))
	if result := callSessionTool(t, session, sessionNewTool, sessionArgs()); result.IsError {
		t.Fatal(resultText(result))
	}
	if starts := callsTo(fake, "agent.start"); len(starts) != 1 {
		t.Fatalf("agent.start calls = %+v", starts)
	}
}

func TestSessionNewClosesATabWhoseShellNeverSettles(t *testing.T) {
	fake := healthyFake()
	fake.newProcesses = []string{newPaneClaude}
	session := connectTestClient(t, newSessionNewServer(t, fake))
	result := callSessionTool(t, session, sessionNewTool, sessionArgs())
	if !result.IsError || !strings.Contains(resultText(result), "closed again") {
		t.Fatalf("result = %s", resultText(result))
	}
	if starts := callsTo(fake, "agent.start"); len(starts) != 0 {
		t.Fatalf("agent.start calls = %+v", starts)
	}
	assertTabClosed(t, fake)
}

func TestSessionNewClosesATabWithoutUsableIDs(t *testing.T) {
	fake := healthyFake()
	fake.tabCreate = `{"type":"tab_created","tab":{"tab_id":"w1:t4"},"root_pane":{"pane_id":"w1:p9","terminal_id":"TERM"}}`
	session := connectTestClient(t, newSessionNewServer(t, fake))
	result := callSessionTool(t, session, sessionNewTool, sessionArgs())
	if !result.IsError || !strings.Contains(resultText(result), "closed again") {
		t.Fatalf("result = %s", resultText(result))
	}
	if starts := callsTo(fake, "agent.start"); len(starts) != 0 {
		t.Fatalf("agent.start calls = %+v", starts)
	}
	assertTabClosed(t, fake)
}

func TestSessionNewNeedsItsOwnFlag(t *testing.T) {
	withActions := listToolNames(t, connectTestClient(t, newAgentToolsServer(t, Options{SessionActions: true})))
	if _, found := withActions[sessionNewTool]; found {
		t.Fatal("session_new is registered by --session-actions alone")
	}
	onlyNew := listToolNames(t, connectTestClient(t, newAgentToolsServer(t, Options{SessionNew: true})))
	tool, found := onlyNew[sessionNewTool]
	if !found {
		t.Fatal("session_new missing with --session-new")
	}
	if _, found := onlyNew["session_clear"]; found {
		t.Fatal("--session-new registered session_clear")
	}
	input, _ := tool.InputSchema.(map[string]any)
	properties, _ := input["properties"].(map[string]any)
	if _, hasTarget := properties["target"]; !hasTarget || len(properties) != 1 {
		t.Fatalf("properties = %v", properties)
	}
	if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.IdempotentHint {
		t.Fatalf("annotations = %+v", tool.Annotations)
	}
}

func TestSessionNewNameIsAValidHerdrName(t *testing.T) {
	name := sessionNewName("term_6602985f0906876")
	if name != "session-6602985f0906876" || !sessionNewNamePattern.MatchString(name) {
		t.Fatalf("name = %s", name)
	}
}
