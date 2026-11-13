package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mrvnklm/herdr-mcp/internal/herdr"
)

const (
	sessionNewTool = "session_new"
	// sessionNewStartTimeout bounds Herdr's own launch of the new Claude.
	sessionNewStartTimeout = 10 * time.Second
	// defaultSessionNewPerHour caps new sessions on top of the shared
	// cooldown: every one is a full Claude process on the host, and a loop at
	// one action per 20 seconds would still start 180 of them an hour.
	defaultSessionNewPerHour = 3
	sessionNewWindow         = time.Hour
	// defaultSessionNewSettle is how long the new tab's shell gets to take the
	// foreground, before the start and again after a failed one. A fresh zsh
	// runs its startup files in the foreground first; Herdr refuses to start
	// an agent until then (agent_pane_busy, measured 2026-11-13), and a tab
	// closed during that would be closed on a guess.
	defaultSessionNewSettle = 5 * time.Second
	// defaultSessionNewReadyWait is how long a launched Claude gets to take
	// input or show a question. Every wait also ends with sessionActionBudget.
	defaultSessionNewReadyWait = 12 * time.Second
	// defaultSessionNewGoneAfter is how long the shell must stay in front
	// after the launch before Claude counts as exited. Right after the launch
	// the shell can still be in front for a moment.
	defaultSessionNewGoneAfter = time.Second
	// sessionNewCloseTimeout gives closing an empty tab its own time, so a
	// spent action budget cannot leave the tab behind.
	sessionNewCloseTimeout = 3 * time.Second
)

var (
	// sessionNewNamePattern is what Herdr accepts as an agent name.
	sessionNewNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	// sessionNewTerminalPattern is the shape of the terminal ids the name is
	// built from; an empty suffix would give every new session the same name.
	sessionNewTerminalPattern = regexp.MustCompile(`^term_[0-9a-z]{1,23}$`)
)

const sessionNewDescription = "Start a new Claude Code session in a new tab, in the same workspace and directory as an existing Claude session. The existing session is not touched. Input: target (pane id of an existing Claude session, such as w1:p2). Returns the pane id of the new session; send its first prompt with agent_prompt."

// registerSessionNew adds session_new. Like the other session tools it sits
// outside register, so Reload leaves it in place and --allow-methods does not
// gate the tab.create and agent.start it calls internally.
func (s *Server) registerSessionNew() {
	notDestructive := false
	closedWorld := false
	tool := &mcp.Tool{
		Name:        sessionNewTool,
		Title:       "Session New",
		Description: sessionNewDescription,
		InputSchema: sessionActionSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			IdempotentHint:  false,
			DestructiveHint: &notDestructive,
			OpenWorldHint:   &closedWorld,
		},
	}
	s.MCP.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started := time.Now()
		result := s.runSessionNew(ctx, request.Params.Arguments)
		s.observe(sessionNewTool, time.Since(started), result)
		return result, nil
	})
}

func (s *Server) runSessionNew(ctx context.Context, arguments json.RawMessage) *mcp.CallToolResult {
	target, err := sessionTarget(arguments)
	if err != nil {
		return errorResult(sessionNewTool, err)
	}
	if !s.sessionMu.TryLock() {
		return errorResult(sessionNewTool, errSessionBusy)
	}
	defer s.sessionMu.Unlock()
	if err := s.sessionCooldownError(); err != nil {
		return errorResult(sessionNewTool, err)
	}
	if err := s.sessionNewQuotaError(time.Now()); err != nil {
		return errorResult(sessionNewTool, err)
	}

	// A client that hangs up must not leave a half-started tab behind
	// without anyone deciding what to do with it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionActionBudget)
	defer cancel()

	workspace, cwd, err := s.sessionNewSource(ctx, target)
	if err != nil {
		return errorResult(sessionNewTool, err)
	}

	created, err := s.Client.Call(ctx, "tab.create", sessionParams(map[string]any{
		"workspace_id": workspace,
		"cwd":          cwd,
		"focus":        false,
	}))
	if err != nil {
		return errorResult(sessionNewTool, fmt.Errorf("Herdr could not open a tab in the workspace of %s: %w", target, err))
	}
	// From here on a tab exists, so this attempt counts against both brakes,
	// whatever happens next.
	defer func() {
		now := time.Now()
		s.lastSessionAction = now
		s.sessionNewStarts = append(s.sessionNewStarts, now)
	}()

	var tab struct {
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID     string `json:"pane_id"`
			TerminalID string `json:"terminal_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(created, &tab); err != nil || tab.Tab.TabID == "" {
		return errorResult(sessionNewTool, fmt.Errorf("Herdr opened a tab in the workspace of %s but did not say which. Nothing was started; tell the user to check the workspace.", target))
	}
	pane := tab.RootPane.PaneID
	name := sessionNewName(tab.RootPane.TerminalID)
	if !sessionTargetPattern.MatchString(pane) || !sessionNewTerminalPattern.MatchString(tab.RootPane.TerminalID) || !sessionNewNamePattern.MatchString(name) {
		return s.closeSessionNewTab(ctx, tab.Tab.TabID, errors.New("Herdr returned no usable pane or terminal id for the new tab"))
	}

	if !s.waitForShell(ctx, pane, s.sessionNewSettle) {
		return s.closeSessionNewTab(ctx, tab.Tab.TabID, errors.New("the shell in the new tab did not become ready"))
	}

	// agent.start types claude plus the operator's arguments into the new
	// shell; nothing from the caller reaches it.
	_, err = s.Client.Call(ctx, "agent.start", sessionParams(map[string]any{
		"name":       name,
		"kind":       "claude",
		"pane_id":    pane,
		"args":       s.sessionNewArgs,
		"timeout_ms": sessionNewStartTimeout.Milliseconds(),
	}))
	var apiErr *herdr.APIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.Code == "agent_not_ready") {
		// Herdr refused or lost the launch. Only a shell back in front proves
		// that nothing runs; anything else may be a Claude still coming up.
		if s.waitForShell(ctx, pane, s.sessionNewSettle) {
			return s.closeSessionNewTab(ctx, tab.Tab.TabID, err)
		}
		return errorResult(sessionNewTool, fmt.Errorf("Claude may still be starting in %s (%v). Check it once with agent_get before sending a prompt; if it is blocked, tell the user.", pane, err))
	}

	// Success over the socket means launched, not ready (measured 2026-11-13:
	// 0.3 s, launch_pending still true, then a trust screen), and
	// agent_not_ready means blocked or already exited. Only the agent's state
	// and the pane's foreground process tell these apart.
	switch s.waitForNewSession(ctx, pane) {
	case newSessionReady:
		return sessionActionResult(fmt.Sprintf("Started a new Claude session in %s (directory %s). It is idle; send its first prompt with agent_prompt and target %s.", pane, cwd, pane))
	case newSessionBlocked:
		return sessionActionResult(fmt.Sprintf("Claude started in %s but is waiting for an answer on its screen, such as trusting the folder, before it accepts prompts. Tell the user; do not answer it and do not prompt it.", pane))
	case newSessionGone:
		return s.closeSessionNewTab(ctx, tab.Tab.TabID, errors.New("Claude exited right after it was launched"))
	default:
		return sessionActionResult(fmt.Sprintf("Claude was launched in %s (directory %s) but was not ready yet. Check it once with agent_get before sending a prompt; if it is blocked, tell the user.", pane, cwd))
	}
}

type newSessionState int

const (
	newSessionPending newSessionState = iota
	newSessionReady
	newSessionBlocked
	newSessionGone
)

// waitForNewSession polls the new pane until Claude takes input, shows a
// question, or has left the shell in front for sessionNewGoneAfter. A status
// only counts while Claude, not the shell, holds the foreground. Read errors
// keep it polling.
func (s *Server) waitForNewSession(ctx context.Context, pane string) newSessionState {
	deadline := time.Now().Add(s.sessionNewReadyWait)
	var shellSince time.Time
	for {
		info, infoErr := s.paneProcesses(ctx, pane)
		switch {
		case infoErr == nil && info.shellInForeground():
			if shellSince.IsZero() {
				shellSince = time.Now()
			}
			if time.Since(shellSince) >= s.sessionNewGoneAfter {
				return newSessionGone
			}
		case infoErr == nil && info.foregroundIsNotShell():
			shellSince = time.Time{}
			if state := s.newSessionStatus(ctx, pane); state != newSessionPending {
				return state
			}
		}
		if time.Now().After(deadline) {
			return newSessionPending
		}
		select {
		case <-ctx.Done():
			return newSessionPending
		case <-time.After(sessionExitPoll):
		}
	}
}

func (s *Server) newSessionStatus(ctx context.Context, pane string) newSessionState {
	result, err := s.Client.Call(ctx, "agent.get", sessionParams(map[string]string{"target": pane}))
	if err != nil {
		return newSessionPending
	}
	var response struct {
		Agent struct {
			PaneID        string  `json:"pane_id"`
			Agent         *string `json:"agent"`
			Status        string  `json:"agent_status"`
			LaunchPending bool    `json:"launch_pending"`
		} `json:"agent"`
	}
	if json.Unmarshal(result, &response) != nil || response.Agent.PaneID != pane ||
		response.Agent.Agent == nil || *response.Agent.Agent != "claude" {
		return newSessionPending
	}
	switch {
	case response.Agent.Status == "blocked":
		return newSessionBlocked
	case response.Agent.Status == "idle" && !response.Agent.LaunchPending:
		return newSessionReady
	}
	return newSessionPending
}

// closeSessionNewTab closes a tab this call opened and nothing runs in. It
// gets its own time: the action budget may be spent by now.
func (s *Server) closeSessionNewTab(ctx context.Context, tabID string, cause error) *mcp.CallToolResult {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionNewCloseTimeout)
	defer cancel()
	if _, err := s.Client.Call(closeCtx, "tab.close", sessionParams(map[string]string{"tab_id": tabID})); err != nil {
		return errorResult(sessionNewTool, fmt.Errorf("Claude did not start (%v), and the empty tab %s could not be closed: %w. Tell the user.", cause, tabID, err))
	}
	return errorResult(sessionNewTool, fmt.Errorf("Claude did not start in the new tab, so the tab was closed again: %w. Tell the user instead of retrying.", cause))
}

// sessionNewSource reads the workspace and directory of a running Claude
// session. Taking both from a session whose Claude is verifiably in front,
// never from the caller, keeps a new session out of directories nobody has
// opened Claude in: a stale agent status would otherwise hand over whatever
// directory the shell moved to after Claude ended.
func (s *Server) sessionNewSource(ctx context.Context, target string) (string, string, error) {
	result, err := s.Client.Call(ctx, "agent.get", sessionParams(map[string]string{"target": target}))
	if err != nil {
		var apiErr *herdr.APIError
		if errors.As(err, &apiErr) && (apiErr.Code == "agent_not_found" || apiErr.Code == "pane_not_found") {
			return "", "", fmt.Errorf("Pane %s does not exist or runs no agent.", target)
		}
		return "", "", fmt.Errorf("Herdr did not report the state of %s: %w", target, err)
	}
	var response struct {
		Agent struct {
			PaneID      string  `json:"pane_id"`
			Agent       *string `json:"agent"`
			WorkspaceID string  `json:"workspace_id"`
			CWD         *string `json:"cwd"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", "", fmt.Errorf("Herdr returned an unreadable response for %s.", target)
	}
	agent := response.Agent
	if agent.Agent == nil || *agent.Agent != "claude" {
		return "", "", fmt.Errorf("%s does not run Claude, so no session was started from it.", target)
	}
	if agent.PaneID != target {
		return "", "", fmt.Errorf("%s is not a current pane id; the agent is in %s. Use agent_list for current ids.", target, agent.PaneID)
	}
	if agent.WorkspaceID == "" {
		return "", "", fmt.Errorf("Herdr did not report the workspace of %s, so no session was started.", target)
	}
	if agent.CWD == nil || !filepath.IsAbs(*agent.CWD) || filepath.Clean(*agent.CWD) != *agent.CWD {
		return "", "", fmt.Errorf("Herdr did not report a usable directory for %s, so no session was started.", target)
	}
	info, err := s.paneProcesses(ctx, target)
	if err != nil {
		return "", "", fmt.Errorf("Herdr did not report the processes of %s: %w", target, err)
	}
	if !info.foregroundIsNotShell() {
		return "", "", fmt.Errorf("Claude is not in the foreground in %s, so no session was started from it.", target)
	}
	return agent.WorkspaceID, *agent.CWD, nil
}

// sessionCooldownError is the spacing every session tool shares. Like every
// session refusal it asks the model to tell the user, not to come back.
func (s *Server) sessionCooldownError() error {
	if s.lastSessionAction.IsZero() || time.Since(s.lastSessionAction) >= s.sessionCooldown {
		return nil
	}
	remaining := s.sessionCooldown - time.Since(s.lastSessionAction)
	return fmt.Errorf("Another session action just ran; the next one is possible in %d seconds. Tell the user instead of retrying.", int(math.Ceil(remaining.Seconds())))
}

// sessionNewQuotaError drops starts older than the window and refuses once
// the window is full. Callers hold sessionMu.
func (s *Server) sessionNewQuotaError(now time.Time) error {
	kept := s.sessionNewStarts[:0]
	for _, started := range s.sessionNewStarts {
		if now.Sub(started) < sessionNewWindow {
			kept = append(kept, started)
		}
	}
	s.sessionNewStarts = kept
	if len(kept) < s.sessionNewPerHour {
		return nil
	}
	wait := sessionNewWindow - now.Sub(kept[0])
	return fmt.Errorf("%d new sessions were started within the last hour, which is the limit. The next one is possible in %d minutes; tell the user instead of retrying.", s.sessionNewPerHour, int(math.Ceil(wait.Minutes())))
}

// sessionNewName derives the agent name Herdr requires from the terminal id
// of the new pane, which is unique and lower-case hex, unlike the mixed-case
// tab and pane ids.
func sessionNewName(terminalID string) string {
	return "session-" + strings.ToLower(strings.TrimPrefix(terminalID, "term_"))
}
