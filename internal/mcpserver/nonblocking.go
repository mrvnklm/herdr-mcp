// Added in this fork. See "What this fork adds" in README.md.

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mrvnklm/herdr-mcp/internal/herdr"
)

// A coding agent's turn routinely runs for minutes. A client such as the
// claude.ai voice mode cannot be notified when it ends, so with --max-wait-ms
// the bridge never makes it wait: an expired agent_wait is a status report,
// not an error, and its note says what to do in exactly that status.
// Measured 2026-11-10: with an error and "call agent_wait again", the client
// called agent_wait 17 times in 9 minutes on an agent that was simply busy.

// nonBlockingInstructions is appended to the server instructions when
// --max-wait-ms is set, so the rule reaches the client before the first call.
const nonBlockingInstructions = `

This bridge does not hold calls open for an agent's turn: agent_prompt returns once the prompt is sent, and agent_wait returns the current status after a short wait instead of an error. Agents often work for several minutes. Never poll in a loop: report the current status to the user and check again only when the user asks.`

// promptSentNote replaces the wait that limitWait drops from agent_prompt.
const promptSentNote = "Prompt sent without waiting; agents often take several minutes. Tell the user it is running and stop here. When the user asks for the result, check agent_get for the status and agent_read for the answer. Do not poll."

// visibleFallbackNote explains why agent_read returned the screen instead of
// the requested scrollback. Herdr refuses only reads longer than the visible
// screen while the agent works (measured 2026-11-11: 60 lines ok, 80 refused).
const visibleFallbackNote = "The agent is working, and Herdr reads more lines than the visible screen only while it is idle, so this is the visible screen (source=visible). Read further back once agent_get reports idle or done."

// waitExpiredNote tells the model what an expired wait means for the status
// agent.get reported afterwards. A blind "still working" would hide a blocked
// agent or a finished answer, which is worse than the polling it replaces.
func waitExpiredNote(status string, until []string, launching bool) string {
	const noLoop = " Do not call agent_wait again in a loop."
	switch {
	case launching:
		return "The agent is still launching and not ready for prompts yet. Tell the user and check again with agent_get only when they ask." + noLoop
	case status == "working":
		return "The agent is still working; agents often take several minutes. Tell the user what it is doing (agent_read with source=visible shows its screen) and check again with agent_get only when they ask." + noLoop
	case status == "blocked":
		return "The agent is blocked: it waits for input or an approval. Tell the user now and let them decide; do not answer for them." + noLoop
	case (status == "idle" || status == "done") && waitsForStart(until):
		return fmt.Sprintf("The agent has not started on the prompt yet (status %s). Tell the user and check again with agent_get only when they ask.", status) + noLoop
	case status == "idle" || status == "done":
		return fmt.Sprintf("The agent has finished (status %s). Read its answer with agent_read now.", status)
	case status == "":
		return "The wait expired and the agent's status could not be read (see status_error). Tell the user." + noLoop
	default:
		return fmt.Sprintf("The wait expired with the agent in status %q. Tell the user and check again with agent_get only when they ask.", status) + noLoop
	}
}

// waitsForStart reports whether until asks only for the start of a turn, as
// right after agent_prompt, so an idle agent means "not started", not "done".
func waitsForStart(until []string) bool {
	if len(until) == 0 {
		return false
	}
	for _, status := range until {
		if status != "working" && status != "blocked" {
			return false
		}
	}
	return true
}

func stringsArgument(arguments json.RawMessage, name string) []string {
	var decoded map[string]any
	if json.Unmarshal(arguments, &decoded) != nil {
		return nil
	}
	values, _ := decoded[name].([]any)
	var result []string
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func apiErrorCode(err error) string {
	var apiError *herdr.APIError
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

func (s *Server) logLine(format string, args ...any) {
	logf := s.Logf
	if logf == nil {
		logf = log.Printf
	}
	logf(format, args...)
}

// waitExpired builds the result of an agent.wait that ran out of time, either
// in Herdr (code "timeout") or while the agent was still launching. It applies
// whenever --max-wait-ms is set, also to a timeout_ms the caller chose below
// the cap: in this mode an expired wait is never an error.
func (s *Server) waitExpired(ctx context.Context, caller herdr.Transport, arguments json.RawMessage, launching bool) (json.RawMessage, string) {
	target := stringArgument(arguments, "target")
	until := stringsArgument(arguments, "until")
	result := map[string]any{"type": "agent_wait", "timed_out": true, "target": target}
	if len(until) > 0 {
		result["until"] = until
	}
	status := ""
	agent, err := s.agentStatus(ctx, caller, target)
	if err != nil {
		result["status_error"] = err.Error()
	} else {
		result["agent"] = agent
		status, _ = agent["agent_status"].(string)
		if pending, _ := agent["launch_pending"].(bool); pending {
			launching = true
		}
	}
	if launching {
		result["launching"] = true
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		encoded = json.RawMessage(`{"type":"agent_wait","timed_out":true}`)
	}
	s.logLine("tool agent.wait expired for %q: status=%q launching=%v until=%v", target, status, launching, until)
	return encoded, waitExpiredNote(status, until, launching)
}

// agentStatus is the part of agent.get that answers "is it done yet".
func (s *Server) agentStatus(ctx context.Context, caller herdr.Transport, target string) (map[string]any, error) {
	if target == "" {
		return nil, errors.New("no target")
	}
	raw, err := caller.Call(ctx, "agent.get", json.RawMessage(fmt.Sprintf(`{"target":%q}`, target)))
	if err != nil {
		return nil, err
	}
	var response struct {
		Agent map[string]any `json:"agent"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.Agent == nil {
		return nil, errors.New("agent.get returned no agent")
	}
	status := map[string]any{}
	for _, key := range []string{"name", "pane_id", "agent_status", "terminal_title_stripped", "cwd", "launch_pending", "interactive_ready"} {
		if value, ok := response.Agent[key]; ok {
			status[key] = value
		}
	}
	return status, nil
}

// remainingWait shrinks timeout_ms by the time a launch already used, so a
// launch plus the wait stays within one --max-wait-ms instead of two.
func remainingWait(arguments json.RawMessage, used time.Duration) json.RawMessage {
	var decoded map[string]any
	if json.Unmarshal(arguments, &decoded) != nil || decoded == nil {
		return arguments
	}
	timeout, ok := decoded["timeout_ms"].(float64)
	if !ok || timeout <= 0 {
		return arguments
	}
	remaining := int(timeout) - int(used.Milliseconds())
	if remaining < 1 {
		remaining = 1
	}
	decoded["timeout_ms"] = remaining
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return arguments
	}
	return encoded
}

// readVisibleInstead retries an agent.read that Herdr refused because the
// agent is busy with source=visible, which works in every state. Like the
// wait handling it belongs to the non-blocking mode that --max-wait-ms turns on.
// If the retry fails too, the caller returns the original error.
func (s *Server) readVisibleInstead(ctx context.Context, caller herdr.Transport, arguments json.RawMessage, err error) (json.RawMessage, bool) {
	if s.maxWaitMS <= 0 || apiErrorCode(err) != "agent_not_idle" {
		return nil, false
	}
	var decoded map[string]any
	if json.Unmarshal(arguments, &decoded) != nil || decoded == nil || decoded["source"] == "visible" {
		return nil, false
	}
	decoded["source"] = "visible"
	params, marshalErr := json.Marshal(decoded)
	if marshalErr != nil {
		return nil, false
	}
	result, retryErr := caller.Call(ctx, "agent.read", params)
	if retryErr != nil {
		s.logLine("tool agent.read fallback to source=visible failed: %v", retryErr)
		return nil, false
	}
	s.logLine("tool agent.read fell back to source=visible for %q", stringArgument(arguments, "target"))
	return result, true
}
