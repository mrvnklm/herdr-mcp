// Added in this fork. See "What this fork adds" in README.md.

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mrvnklm/herdr-mcp/internal/core"
)

// The herdr desktop app keeps its sessions in herdr-core, a separate terminal
// host the herdr TUI socket knows nothing about. Measured 2026-11-19 on the
// studio: about 20 Claude sessions under herdr server, about 18 under
// herdr-core, all of the latter invisible to agent_list. The app_* tools reach
// them through the hc command line with the same rules as agent_*: read
// freely, prompt only an agent that takes input, never answer its dialog.

const (
	appListTool   = "app_list"
	appGetTool    = "app_get"
	appReadTool   = "app_read"
	appWaitTool   = "app_wait"
	appPromptTool = "app_prompt"

	// appDefaultWait applies when --max-wait-ms is not set; appMaxWait caps
	// it either way. Plus the 10 s margin of the hc call it stays below the
	// roughly 40 s after which a remote MCP client gives up.
	appDefaultWait = 25 * time.Second
	appMaxWait     = 25 * time.Second
	// appPromptsPerMinute is the brake on a prompt injection that walks
	// every session; a person talking to the app sends far fewer.
	appPromptsPerMinute = 6
	// appDefaultLines and appMaxLines apply when --read-default-lines and
	// --read-max-lines are not set.
	appDefaultLines = 60
	appMaxLines     = 200
	// appMaxPromptRunes bounds one prompt; a voice prompt is far shorter.
	appMaxPromptRunes = 8000
)

const appInstructions = `

Sessions of the herdr desktop app run in herdr-core and are reached only with the app_* tools: app_list, app_get, app_read, app_wait, app_prompt. Their targets look like t1w. The agent_* and session_* tools reach only herdr terminal panes, whose targets look like w1:p2. When the user names a session, look in both lists.`

const appPromptSentNote = "Prompt sent without waiting; agents often take several minutes. Tell the user it is running and stop here. When the user asks for the result, check app_get for the status and app_read for the answer. Do not poll."

var appTargetSchema = map[string]any{
	"type":        "string",
	"description": "Terminal address from app_list, such as t1w.",
}

func appSchema(properties map[string]any, required ...string) map[string]any {
	requiredList := make([]any, 0, len(required))
	for _, name := range required {
		requiredList = append(requiredList, name)
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             requiredList,
		"additionalProperties": false,
	}
}

// registerAppTools adds the app_* tools. Like the session tools they sit
// outside register, so Reload leaves them in place and --allow-methods does
// not apply to them.
func (s *Server) registerAppTools() {
	s.addAppTool(appListTool, "agent.list",
		"List the coding agent sessions in the herdr desktop app (herdr-core), with target, status (idle, working, blocked, done, error), place in the app (space / tab) and directory. These are not in agent_list.",
		appSchema(map[string]any{}), s.appList)
	s.addAppTool(appGetTool, "agent.get",
		"Show one herdr desktop app session: status, what it is waiting for, place in the app, title and directory. Input: target from app_list.",
		appSchema(map[string]any{"target": appTargetSchema}, "target"), s.appGet)
	s.addAppTool(appReadTool, "agent.read",
		"Read the screen of a herdr desktop app session, plus lines of history above it. While the agent works or is blocked only the screen is returned. Input: target, lines (optional).",
		appSchema(map[string]any{
			"target": appTargetSchema,
			"lines": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": fmt.Sprintf("Lines of history above the screen, at most %d.", s.appMaxLines()),
			},
		}, "target"), s.appRead)
	s.addAppTool(appWaitTool, "agent.wait",
		"Wait briefly for a herdr desktop app session to stop working, then report its status. It returns the status when the wait runs out instead of failing; do not call it in a loop. Input: target.",
		appSchema(map[string]any{"target": appTargetSchema}, "target"), s.appWait)
	s.addAppTool(appPromptTool, "agent.prompt",
		"Send a prompt to a herdr desktop app session that is idle or done, as if the user typed it. It refuses a session that is working or waits for an answer on its screen, and anything that is not a coding agent. Input: target, text.",
		appSchema(map[string]any{
			"target": appTargetSchema,
			"text": map[string]any{
				"type":        "string",
				"minLength":   1,
				"maxLength":   appMaxPromptRunes,
				"description": "The prompt. Line breaks are allowed, other control characters are not.",
			},
		}, "target", "text"), s.appPrompt)
}

func (s *Server) addAppTool(name, likeMethod, description string, schema map[string]any, run func(context.Context, json.RawMessage) *mcp.CallToolResult) {
	tool := &mcp.Tool{
		Name:        name,
		Title:       toolTitle(name),
		Description: description,
		InputSchema: schema,
		Annotations: annotations(likeMethod),
	}
	s.MCP.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started := time.Now()
		result := run(ctx, request.Params.Arguments)
		s.observe(name, time.Since(started), result)
		return result, nil
	})
}

type appArguments struct {
	Target string  `json:"target"`
	Lines  *int    `json:"lines"`
	Text   *string `json:"text"`
}

// decodeAppArguments rejects unknown fields and any target that is not a
// plain local terminal address, before hc is called at all.
func decodeAppArguments(tool string, arguments json.RawMessage, allowed ...string) (appArguments, error) {
	var decoded appArguments
	var raw map[string]json.RawMessage
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage("{}")
	}
	if err := json.Unmarshal(arguments, &raw); err != nil {
		return decoded, fmt.Errorf("%s takes a JSON object", tool)
	}
	for name := range raw {
		if !contains(allowed, name) {
			return decoded, fmt.Errorf("%s does not take %q", tool, name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	if err := decoder.Decode(&decoded); err != nil {
		return decoded, fmt.Errorf("%s: %v", tool, err)
	}
	if contains(allowed, "target") && !core.AddressPattern.MatchString(decoded.Target) {
		return decoded, errors.New("target must be exactly one terminal address from app_list, such as t1w.")
	}
	return decoded, nil
}

func contains(list []string, value string) bool {
	for _, entry := range list {
		if entry == value {
			return true
		}
	}
	return false
}

type appSession struct {
	Target  string `json:"target"`
	Agent   string `json:"agent"`
	Status  string `json:"status"`
	Waiting string `json:"waiting_for,omitempty"`
	Where   string `json:"where,omitempty"`
	Title   string `json:"title,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Turn    int    `json:"turn"`
}

func (s *Server) appList(ctx context.Context, arguments json.RawMessage) *mcp.CallToolResult {
	if _, err := decodeAppArguments(appListTool, arguments); err != nil {
		return errorResult(appListTool, err)
	}
	entries, err := s.core.List(ctx)
	if err != nil {
		return errorResult(appListTool, fmt.Errorf("herdr-core did not list its sessions: %w", err))
	}
	sessions := []appSession{}
	for _, entry := range entries {
		// A terminal without an agent status is a plain program or shell:
		// nothing app_prompt may type into, so it is not listed either.
		if entry.State != "live" || entry.Status == "" || !core.AddressPattern.MatchString(entry.Address) {
			continue
		}
		sessions = append(sessions, appSession{
			Target: entry.Address, Agent: entry.Agent, Status: entry.Status,
			Where: entry.Where, Cwd: entry.Cwd, Turn: entry.Turn,
		})
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Where < sessions[j].Where })
	return appJSONResult(map[string]any{"sessions": sessions})
}

func (s *Server) appGet(ctx context.Context, arguments json.RawMessage) *mcp.CallToolResult {
	decoded, err := decodeAppArguments(appGetTool, arguments, "target")
	if err != nil {
		return errorResult(appGetTool, err)
	}
	terminal, err := s.core.Get(ctx, decoded.Target)
	if err != nil {
		return errorResult(appGetTool, appCoreError(decoded.Target, err))
	}
	if terminal.State != "live" {
		return errorResult(appGetTool, fmt.Errorf("%s has finished; it is no longer a running session.", decoded.Target))
	}
	if terminal.AgentState() == "" {
		return errorResult(appGetTool, fmt.Errorf("%s runs no coding agent.", decoded.Target))
	}
	return appJSONResult(sessionFromTerminal(terminal))
}

func sessionFromTerminal(terminal core.Terminal) appSession {
	session := appSession{
		Target: terminal.Address, Agent: terminal.Status.App, Status: terminal.Status.State,
		Where: terminal.Labels["where"], Title: terminal.Title, Cwd: terminal.Cwd, Turn: terminal.Turn,
	}
	if terminal.Status.State == "blocked" {
		session.Waiting = terminal.Status.Message
	}
	return session
}

func (s *Server) appRead(ctx context.Context, arguments json.RawMessage) *mcp.CallToolResult {
	decoded, err := decodeAppArguments(appReadTool, arguments, "target", "lines")
	if err != nil {
		return errorResult(appReadTool, err)
	}
	lines := appDefaultLines
	if s.readDefaultLines > 0 {
		lines = s.readDefaultLines
	}
	if decoded.Lines != nil {
		lines = *decoded.Lines
	}
	if maxLines := s.appMaxLines(); lines > maxLines {
		lines = maxLines
	}
	if lines < 0 {
		lines = 0
	}
	screen, err := s.core.Read(ctx, decoded.Target, lines)
	if err != nil {
		return errorResult(appReadTool, appCoreError(decoded.Target, err))
	}
	content := []mcp.Content{&mcp.TextContent{Text: screen.Text}}
	if !screen.Complete {
		why := screen.Why
		if why == "" {
			why = "busy"
		}
		content = append(content, &mcp.TextContent{Text: fmt.Sprintf("The session is %s, so this is only its screen without history. Read further back once app_get reports idle or done.", why)})
	}
	return &mcp.CallToolResult{Content: content}
}

// appMaxLines is the history cap: --read-max-lines when set.
func (s *Server) appMaxLines() int {
	if s.readMaxLines > 0 {
		return s.readMaxLines
	}
	return appMaxLines
}

func (s *Server) appWait(ctx context.Context, arguments json.RawMessage) *mcp.CallToolResult {
	decoded, err := decodeAppArguments(appWaitTool, arguments, "target")
	if err != nil {
		return errorResult(appWaitTool, err)
	}
	wait := appDefaultWait
	if s.maxWaitMS > 0 {
		wait = time.Duration(s.maxWaitMS) * time.Millisecond
	}
	if wait > appMaxWait {
		wait = appMaxWait
	}
	result, err := s.core.Wait(ctx, decoded.Target, wait)
	if err != nil {
		return errorResult(appWaitTool, appCoreError(decoded.Target, err))
	}
	payload := map[string]any{"target": decoded.Target, "status": result.Status, "reason": result.Reason, "turn": result.Turn}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: mustJSON(payload)},
		&mcp.TextContent{Text: appWaitNote(result)},
	}}
}

func appWaitNote(result core.WaitResult) string {
	const noLoop = " Do not call app_wait again in a loop."
	switch {
	case result.Exited():
		return "The session has ended. Tell the user."
	case result.Status == "":
		return "The session runs no coding agent any more. Tell the user."
	case result.Status == "working":
		return "The agent is still working; agents often take several minutes. Tell the user what it is doing (app_read shows its screen) and check again with app_get only when they ask." + noLoop
	case result.Status == "blocked":
		return "The agent is blocked: it waits for an answer or an approval on its screen. Tell the user now and let them decide; do not answer for them." + noLoop
	case result.Status == "idle" || result.Status == "done":
		return fmt.Sprintf("The agent has stopped (status %s). Read its answer with app_read now.", result.Status)
	default:
		return fmt.Sprintf("The agent is in status %q. Tell the user and check again with app_get only when they ask.", result.Status) + noLoop
	}
}

func (s *Server) appPrompt(ctx context.Context, arguments json.RawMessage) *mcp.CallToolResult {
	decoded, err := decodeAppArguments(appPromptTool, arguments, "target", "text")
	if err != nil {
		return errorResult(appPromptTool, err)
	}
	text, err := appPromptText(decoded.Text)
	if err != nil {
		return errorResult(appPromptTool, err)
	}
	if err := s.appPromptAllowed(time.Now()); err != nil {
		return errorResult(appPromptTool, err)
	}
	terminal, err := s.core.Get(ctx, decoded.Target)
	if err != nil {
		return errorResult(appPromptTool, appCoreError(decoded.Target, err))
	}
	if terminal.State != "live" {
		return errorResult(appPromptTool, fmt.Errorf("%s has finished. Nothing was sent; tell the user.", decoded.Target))
	}
	// Without an agent status the terminal runs a shell or a plain program,
	// where the prompt and its Enter would run as a command.
	state := terminal.AgentState()
	switch state {
	case "idle", "done":
	case "":
		return errorResult(appPromptTool, fmt.Errorf("%s runs no coding agent. Nothing was sent; tell the user.", decoded.Target))
	case "working":
		return errorResult(appPromptTool, fmt.Errorf("%s is still working. Nothing was sent; tell the user and send it once app_get reports idle or done.", decoded.Target))
	case "blocked":
		return errorResult(appPromptTool, fmt.Errorf("%s waits for an answer on its screen (%s). Nothing was sent; tell the user and do not answer for them.", decoded.Target, terminal.Status.Message))
	default:
		return errorResult(appPromptTool, fmt.Errorf("%s is in status %q. Nothing was sent; tell the user.", decoded.Target, state))
	}
	// The status word outlives Claude when it exits into a nested shell, so
	// the foreground program decides.
	if !terminal.RunsClaude() {
		return errorResult(appPromptTool, fmt.Errorf("Claude is not the foreground program of %s any more. Nothing was sent; tell the user.", decoded.Target))
	}
	// hc types into an idle agent only while it is still idle. "done" has no
	// such guard in hc (--if takes only idle or working, 0.1.21); hc still
	// refuses a blocked agent on its own. Between the check above and the
	// typing, which waits for about 3 s of quiet, Claude could exit; that
	// window stays open until hc can check "done" itself.
	sent, err := s.core.Send(ctx, decoded.Target, text, state == "idle")
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			s.logLine("tool %s timed out sending %d characters to %s", appPromptTool, utf8.RuneCountInString(text), decoded.Target)
			return errorResult(appPromptTool, fmt.Errorf("herdr-core did not confirm in time; the prompt to %s is probably still being delivered (a half-typed draft can hold it for up to a minute). Do not send it again. Tell the user and check app_read in a minute.", decoded.Target))
		case contains([]string{"blocked", "status_mismatch", "finished", "not_found"}, core.Kind(err)):
			return errorResult(appPromptTool, fmt.Errorf("Nothing was sent to %s: %v. Tell the user instead of retrying.", decoded.Target, err))
		}
		return errorResult(appPromptTool, fmt.Errorf("Sending to %s failed (%v); it may still have arrived. Check app_read before sending again.", decoded.Target, err))
	}
	s.logLine("tool %s sent %d characters to %s (status %s, started %s)", appPromptTool, utf8.RuneCountInString(text), decoded.Target, optional(sent.Status), optional(sent.Started))
	payload := map[string]any{"target": decoded.Target, "sent": true, "status": sent.Status, "started": sent.Started}
	note := appPromptSentNote
	switch {
	case sent.Status == nil || (*sent.Status != "idle" && *sent.Status != "done"):
		note = fmt.Sprintf("The text went in while %s reported status %s, not idle or done. Tell the user exactly that and check app_read; do not send it again.", decoded.Target, optional(sent.Status))
	case sent.Started != nil && !*sent.Started:
		note = "The text went in but the agent did not start working within a few seconds. Check app_read once and tell the user what it shows; do not send it again."
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: mustJSON(payload)},
		&mcp.TextContent{Text: note},
	}}
}

// appPromptAllowed counts the app_prompt calls of the last minute, refused
// ones included, against appPromptsPerMinute.
func (s *Server) appPromptAllowed(now time.Time) error {
	s.appPromptMu.Lock()
	defer s.appPromptMu.Unlock()
	recent := s.appPromptTimes[:0]
	for _, at := range s.appPromptTimes {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	s.appPromptTimes = recent
	if len(recent) >= appPromptsPerMinute {
		return fmt.Errorf("%d prompts in the last minute is the limit. Nothing was sent; tell the user.", appPromptsPerMinute)
	}
	s.appPromptTimes = append(s.appPromptTimes, now)
	return nil
}

// optional renders a pointer for a log line or a note.
func optional[T any](value *T) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprint(*value)
}

// appPromptText allows line breaks and tabs but no other control characters:
// an escape sequence in a pasted prompt could steer the terminal itself.
func appPromptText(text *string) (string, error) {
	if text == nil || strings.TrimSpace(*text) == "" {
		return "", errors.New("text must not be empty.")
	}
	if utf8.RuneCountInString(*text) > appMaxPromptRunes {
		return "", fmt.Errorf("text is longer than %d characters.", appMaxPromptRunes)
	}
	if !utf8.ValidString(*text) || strings.IndexFunc(*text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t'
	}) >= 0 {
		return "", errors.New("text may contain line breaks but no other control characters.")
	}
	// In Claude Code a leading ! runs a shell command, / opens a command or
	// picker that the next prompt would answer, and # writes to memory.
	if first := strings.TrimLeftFunc(*text, unicode.IsSpace)[0]; first == '!' || first == '/' || first == '#' {
		return "", fmt.Errorf("text must not start with %q: Claude Code treats that as a command, not a prompt. Start with a word.", first)
	}
	return *text, nil
}

func appCoreError(target string, err error) error {
	switch core.Kind(err) {
	case "not_found":
		return fmt.Errorf("There is no session %s in the herdr desktop app; check app_list.", target)
	case "finished":
		return fmt.Errorf("%s has finished; it is no longer a running session.", target)
	}
	return fmt.Errorf("herdr-core did not answer for %s: %w", target, err)
}

func appJSONResult(value any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(value)}}}
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(encoded)
}
