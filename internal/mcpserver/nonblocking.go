// Added in this fork. See "What this fork adds" in README.md.

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mrvnklm/herdr-mcp/internal/herdr"
)

// A coding agent's turn routinely runs for minutes. A client such as the
// claude.ai voice mode cannot be notified when it ends, so with --max-wait-ms
// the bridge never makes it wait: an expired agent_wait is a status report,
// not an error, and every hint says to check again when the user asks.
// Measured 2026-11-10: with an error and "call agent_wait again", the client
// called agent_wait 17 times in 9 minutes on an agent that was simply busy.

// stillWorkingNote ends a turn instead of starting a polling loop.
const stillWorkingNote = "The agent is still working; agents often take several minutes. Do not call agent_wait again in a loop. Tell the user what the agent is doing (agent_read with source=visible shows its screen) and check again with agent_get only when the user asks."

// promptSentNote replaces the wait that limitWait drops from agent_prompt.
const promptSentNote = "Prompt sent without waiting; agents often take several minutes. Tell the user it is running and stop here. When the user asks for the result, check agent_get for the status and agent_read for the answer. Do not poll."

// nonBlockingInstructions is appended to the server instructions when
// --max-wait-ms is set, so the rule reaches the client before the first call.
const nonBlockingInstructions = `

This bridge does not hold calls open for an agent's turn: agent_prompt returns once the prompt is sent, and agent_wait returns the current status after a short wait instead of an error. Agents often work for several minutes. Never poll in a loop: report the current status to the user and check again only when the user asks.`

// visibleFallbackNote explains why agent_read returned the screen instead of
// the requested scrollback.
const visibleFallbackNote = "The agent is working, and its scrollback can only be read while it is idle, so this is the visible screen (source=visible). Read the full answer once agent_get reports idle or done."

func apiErrorCode(err error) string {
	var apiError *herdr.APIError
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

// waitTimedOut turns an expired agent.wait into the agent's current status.
// It only applies when the bridge itself bounded the wait (--max-wait-ms);
// otherwise the caller chose the timeout and gets Herdr's error unchanged.
func (s *Server) waitTimedOut(ctx context.Context, caller herdr.Transport, arguments json.RawMessage, err error) (json.RawMessage, bool) {
	if s.maxWaitMS <= 0 || apiErrorCode(err) != "timeout" {
		return nil, false
	}
	target := stringArgument(arguments, "target")
	result := map[string]any{"type": "agent_wait", "timed_out": true, "target": target}
	if target != "" {
		if agent, getErr := s.agentStatus(ctx, caller, target); getErr == nil {
			result["agent"] = agent
		}
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return nil, false
	}
	return encoded, true
}

// agentStatus is the part of agent.get that answers "is it done yet".
func (s *Server) agentStatus(ctx context.Context, caller herdr.Transport, target string) (map[string]any, error) {
	raw, err := caller.Call(ctx, "agent.get", json.RawMessage(fmt.Sprintf(`{"target":%q}`, target)))
	if err != nil {
		return nil, err
	}
	var response struct {
		Agent map[string]any `json:"agent"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.Agent == nil {
		return nil, fmt.Errorf("agent.get returned no agent")
	}
	status := map[string]any{}
	for _, key := range []string{"name", "pane_id", "agent_status", "terminal_title_stripped", "cwd"} {
		if value, ok := response.Agent[key]; ok {
			status[key] = value
		}
	}
	return status, nil
}

// readVisibleInstead retries an agent.read that Herdr refused because the
// agent is busy with source=visible, which works in every state. Like the
// wait handling it belongs to the non-blocking mode that --max-wait-ms turns on.
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
		return nil, false
	}
	return result, true
}
