package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// sessionActionBudget bounds one action end to end. Clients abandon a tool
	// call after roughly 40 seconds, and the action must be over before that.
	sessionActionBudget = 23 * time.Second
	// defaultSessionExitWait is the share of that budget session_restart waits
	// for the shell. SessionEnd hooks can make Claude Code slow to exit.
	defaultSessionExitWait = 20 * time.Second
	sessionExitPoll        = 200 * time.Millisecond
	// defaultSessionCooldown is the only brake on a prompt injection that walks
	// every idle pane from agent_list: each action alone looks harmless.
	defaultSessionCooldown = 20 * time.Second

	// sessionDraftClearKeys is how many ctrl+u a draft gets. One press removes
	// one line of a multi-line composer and the next joins the lines, so this
	// covers a draft of about 32 lines. Extra presses on an empty composer do
	// nothing.
	sessionDraftClearKeys = 64

	sessionRestartTool = "session_restart"
)

var (
	sessionTargetPattern = regexp.MustCompile(`^w[0-9A-Za-z]+:p[0-9A-Za-z]+$`)
	sessionUUIDPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	errSessionBusy = errors.New("Gerade lief schon eine Session-Aktion, versuch es gleich nochmal.")
)

// sessionAction is one fixed tool. command is the only text it can ever send,
// so nothing a caller passes reaches a terminal.
type sessionAction struct {
	tool, title, description, command string
}

var sessionActions = []sessionAction{
	{
		tool:        "session_clear",
		title:       "Session Clear",
		command:     "/clear",
		description: "Send /clear to an idle Claude Code session, discarding its conversation. Rejected unless the pane runs Claude and its status is idle or done. Input: target (pane id such as w1:p2).",
	},
	{
		tool:        "session_compact",
		title:       "Session Compact",
		command:     "/compact",
		description: "Send /compact to an idle Claude Code session and return without waiting for it to finish. Rejected unless the pane runs Claude and its status is idle or done. Input: target (pane id such as w1:p2).",
	},
	{
		tool:        sessionRestartTool,
		title:       "Session Restart",
		command:     "/exit",
		description: "Exit an idle Claude Code session and start it again in the same pane with claude --resume, keeping its conversation. Rejected unless the pane runs Claude and its status is idle or done. Input: target (pane id such as w1:p2).",
	},
}

var sessionActionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"target": map[string]any{
			"type":        "string",
			"description": "Pane id of the session, such as w1:p2.",
		},
	},
	"required":             []any{"target"},
	"additionalProperties": false,
}

// registerSessionActions adds the fixed tools once. Like machine_list they are
// outside register, which keeps them off the machine argument and the schema
// allow-list and lets Reload leave them in place.
func (s *Server) registerSessionActions() {
	destructive := true
	closedWorld := false
	for _, action := range sessionActions {
		tool := &mcp.Tool{
			Name:        action.tool,
			Title:       action.title,
			Description: action.description,
			InputSchema: sessionActionSchema,
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				IdempotentHint:  false,
				DestructiveHint: &destructive,
				OpenWorldHint:   &closedWorld,
			},
		}
		s.MCP.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			started := time.Now()
			result := s.runSessionAction(ctx, action, request.Params.Arguments)
			s.observe(action.tool, time.Since(started), result)
			return result, nil
		})
	}
}

func (s *Server) runSessionAction(ctx context.Context, action sessionAction, arguments json.RawMessage) *mcp.CallToolResult {
	target, err := sessionTarget(arguments)
	if err != nil {
		return errorResult(action.tool, err)
	}
	if !s.sessionMu.TryLock() {
		return errorResult(action.tool, errSessionBusy)
	}
	defer s.sessionMu.Unlock()
	if !s.lastSessionAction.IsZero() && time.Since(s.lastSessionAction) < s.sessionCooldown {
		return errorResult(action.tool, errSessionBusy)
	}

	// A client that hangs up after /exit must not cancel the resume, or the
	// pane is left without Claude and nobody is told.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionActionBudget)
	defer cancel()

	sessionID, err := s.checkSessionTarget(ctx, action, target)
	if err != nil {
		return errorResult(action.tool, err)
	}

	// Rejections above do not count; from here on the pane may change.
	defer func() { s.lastSessionAction = time.Now() }()

	if err := s.submitSlashCommand(ctx, target, action.command); err != nil {
		return errorResult(action.tool, fmt.Errorf("%s konnte nicht an %s geschickt werden: %w", action.command, target, err))
	}
	if action.tool != sessionRestartTool {
		return sessionActionResult(fmt.Sprintf("%s ist an %s geschickt.", action.command, target))
	}

	// Typed while Claude still runs, this line would land as a prompt in the
	// session, so only a positive shell signal may lead past this point.
	if !s.waitForShell(ctx, target) {
		return errorResult(action.tool, fmt.Errorf("/exit ist an %s geschickt, aber die Shell kam nicht rechtzeitig zurück. Claude wurde nicht neu gestartet und ich habe nichts weiter getippt, schau bitte selbst nach.", target))
	}
	if _, err := s.Client.Call(ctx, "pane.send_input", sessionParams(map[string]any{
		"pane_id": target,
		"text":    "claude --resume " + sessionID,
		"keys":    []string{"enter"},
	})); err != nil {
		return errorResult(action.tool, fmt.Errorf("Claude in %s ist beendet, aber der Neustart ging nicht raus: %w", target, err))
	}
	return sessionActionResult(fmt.Sprintf("Claude in %s wurde beendet und startet mit derselben Session neu.", target))
}

// sessionTarget accepts exactly one argument, a pane id. Agent names are out:
// they can move between panes, and the id is later part of what gets checked.
func sessionTarget(arguments json.RawMessage) (string, error) {
	var decoded struct {
		Target string `json:"target"`
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil || !sessionTargetPattern.MatchString(decoded.Target) {
		return "", errors.New("Ich brauche genau eine Pane-ID wie w1:p2 als target, sonst nichts.")
	}
	return decoded.Target, nil
}

// checkSessionTarget runs every check that needs no input to the pane and, for
// session_restart, returns the validated session id.
func (s *Server) checkSessionTarget(ctx context.Context, action sessionAction, target string) (string, error) {
	result, err := s.Client.Call(ctx, "agent.get", sessionParams(map[string]string{"target": target}))
	if err != nil {
		var apiErr *herdr.APIError
		if errors.As(err, &apiErr) && (apiErr.Code == "agent_not_found" || apiErr.Code == "pane_not_found") {
			return "", fmt.Errorf("Pane %s gibt es nicht oder dort läuft kein Agent.", target)
		}
		return "", fmt.Errorf("Herdr hat den Zustand von %s nicht geliefert: %w", target, err)
	}
	var response struct {
		Agent struct {
			PaneID  string  `json:"pane_id"`
			Agent   *string `json:"agent"`
			Status  string  `json:"agent_status"`
			Session *struct {
				Agent string `json:"agent"`
				Kind  string `json:"kind"`
				Value string `json:"value"`
			} `json:"agent_session"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", fmt.Errorf("Herdr hat für %s eine unlesbare Antwort geliefert.", target)
	}
	agent := response.Agent
	switch agent.Status {
	case "idle", "done":
	case "working":
		return "", fmt.Errorf("Claude in %s arbeitet gerade, ich fasse die Session nicht an.", target)
	case "blocked":
		return "", fmt.Errorf("Claude in %s wartet gerade auf eine Antwort, ich fasse die Session nicht an.", target)
	default:
		return "", fmt.Errorf("Der Zustand von %s ist unbekannt, ich fasse die Session nicht an.", target)
	}
	if agent.Agent == nil || *agent.Agent != "claude" {
		return "", fmt.Errorf("In %s läuft kein Claude, ich fasse die Session nicht an.", target)
	}
	if agent.PaneID != target {
		return "", fmt.Errorf("%s ist nicht die Pane-ID dieses Agents.", target)
	}

	var sessionID string
	if action.tool == sessionRestartTool {
		session := agent.Session
		if session == nil || session.Agent != "claude" || session.Kind != "id" || !sessionUUIDPattern.MatchString(session.Value) {
			return "", fmt.Errorf("Für %s gibt es keine gültige Claude-Session-ID, ich starte nichts neu.", target)
		}
		sessionID = session.Value
	}

	// A stale agent status must not pass for a running Claude: the exit poll
	// would then succeed at once, without /exit having done anything.
	info, err := s.paneProcesses(ctx, target)
	if err != nil {
		return "", fmt.Errorf("Herdr hat die Prozesse von %s nicht geliefert: %w", target, err)
	}
	if !info.foregroundIsNotShell() {
		return "", fmt.Errorf("In %s läuft Claude nicht im Vordergrund, ich sende nichts.", target)
	}
	return sessionID, nil
}

// submitSlashCommand clears an unsent draft first, which would otherwise be
// submitted as a prompt with the command glued to its end. agent.prompt sends
// text and Enter as one submission and refuses a blocked agent server-side.
func (s *Server) submitSlashCommand(ctx context.Context, target, command string) error {
	keys := make([]string, sessionDraftClearKeys)
	for index := range keys {
		keys[index] = "ctrl+u"
	}
	if _, err := s.Client.Call(ctx, "pane.send_keys", sessionParams(map[string]any{
		"pane_id": target,
		"keys":    keys,
	})); err != nil {
		return err
	}
	_, err := s.Client.Call(ctx, "agent.prompt", sessionParams(map[string]string{
		"target": target,
		"text":   command,
	}))
	return err
}

// waitForShell reports whether the pane's shell took the foreground back within
// the exit budget. Transient read errors just keep it polling.
func (s *Server) waitForShell(ctx context.Context, target string) bool {
	deadline := time.Now().Add(s.sessionExitWait)
	for {
		if info, err := s.paneProcesses(ctx, target); err == nil && info.shellInForeground() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(sessionExitPoll):
		}
	}
}

// paneProcessInfo keeps the id fields nullable on purpose: decoded into plain
// integers a null would become 0, and 0 == 0 would read as "shell in front".
type paneProcessInfo struct {
	ShellPID        *uint32 `json:"shell_pid"`
	ForegroundGroup *uint32 `json:"foreground_process_group_id"`
	Foreground      []struct {
		PID uint32 `json:"pid"`
	} `json:"foreground_processes"`
}

func (i paneProcessInfo) foregroundIsNotShell() bool {
	return i.ShellPID != nil && i.ForegroundGroup != nil && *i.ShellPID != *i.ForegroundGroup
}

func (i paneProcessInfo) shellInForeground() bool {
	return i.ShellPID != nil && i.ForegroundGroup != nil && *i.ShellPID == *i.ForegroundGroup &&
		len(i.Foreground) == 1 && i.Foreground[0].PID == *i.ShellPID
}

func (s *Server) paneProcesses(ctx context.Context, target string) (paneProcessInfo, error) {
	result, err := s.Client.Call(ctx, "pane.process_info", sessionParams(map[string]string{"pane_id": target}))
	if err != nil {
		return paneProcessInfo{}, err
	}
	var response struct {
		ProcessInfo paneProcessInfo `json:"process_info"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return paneProcessInfo{}, err
	}
	return response.ProcessInfo, nil
}

func sessionParams(params any) json.RawMessage {
	encoded, err := json.Marshal(params)
	if err != nil {
		// Maps of strings cannot fail to encode.
		panic(err)
	}
	return encoded
}

func sessionActionResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
