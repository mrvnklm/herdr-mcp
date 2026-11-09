// Modified from Orange-County-AI/herdr-mcp in this fork. See "What this fork adds" in README.md.

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
)

const defaultAgentStartupTimeout = 30 * time.Second

func (s *Server) waitForStartedAgent(ctx context.Context, caller herdr.Transport, startResult json.RawMessage, arguments json.RawMessage) (json.RawMessage, string, error) {
	target := stringArgument(arguments, "name")
	if target == "" {
		return startResult, "", nil
	}
	agent, err := s.waitForAgentReady(ctx, caller, target, argumentTimeout(arguments, defaultAgentStartupTimeout))
	if err != nil {
		return startResult, fmt.Sprintf("Agent %q started but is still launching. Use agent_wait with target %q before agent_prompt; its pane is available for raw terminal input.", target, target), nil
	}
	var result map[string]any
	if json.Unmarshal(startResult, &result) != nil {
		return startResult, "", nil
	}
	result["agent"] = agent
	updated, err := json.Marshal(result)
	if err != nil {
		return startResult, "", nil
	}
	return updated, "", nil
}

func (s *Server) waitThroughLaunch(ctx context.Context, caller herdr.Transport, arguments json.RawMessage) error {
	target := stringArgument(arguments, "target")
	if target == "" {
		return nil
	}
	launching, _, err := s.launchingAgent(ctx, caller, target)
	if err != nil || !launching {
		return nil
	}
	_, err = s.waitForAgentReady(ctx, caller, target, argumentTimeout(arguments, defaultAgentStartupTimeout))
	if err != nil {
		return fmt.Errorf("agent %q is still launching; %w", target, err)
	}
	return nil
}

// limitWait drops the wait of agent.prompt and forces timeout_ms on agent.wait
// to a positive integer no larger
// than the configured maximum. Anything else, 0 and null included, would fall
// through to the socket's 15-minute budget. It runs before waitThroughLaunch,
// which reads the same argument.
func (s *Server) limitWait(method string, params json.RawMessage) (json.RawMessage, string) {
	if (method != "agent.wait" && method != "agent.prompt") || s.maxWaitMS <= 0 {
		return params, ""
	}
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil || decoded == nil {
		return params, ""
	}
	if method == "agent.prompt" {
		// A waiting prompt runs for as long as the agent's turn, far past what a
		// bridge with a wait limit can hold open. The prompt is sent either way;
		// only the wait is dropped.
		if decoded["wait"] == nil {
			return params, ""
		}
		delete(decoded, "wait")
		encoded, err := json.Marshal(decoded)
		if err != nil {
			return params, ""
		}
		return encoded, "Sent without waiting: this bridge does not hold a call open for a whole turn. Use agent_wait, then agent_read for the answer."
	}
	requested, isNumber := decoded["timeout_ms"].(float64)
	if isNumber && requested > 0 && requested <= float64(s.maxWaitMS) && requested == math.Trunc(requested) {
		return params, ""
	}
	decoded["timeout_ms"] = s.maxWaitMS
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return params, ""
	}
	note := ""
	if isNumber && requested > float64(s.maxWaitMS) {
		note = fmt.Sprintf("Capped timeout_ms at %d (requested %v): this bridge waits at most %d ms per call. Call agent_wait again to keep waiting.", s.maxWaitMS, requested, s.maxWaitMS)
	}
	return encoded, note
}

func (s *Server) enrichAgentError(ctx context.Context, caller herdr.Transport, method string, arguments json.RawMessage, err error) error {
	target := stringArgument(arguments, "target")
	if target == "" {
		return err
	}
	launching, paneID, lookupErr := s.launchingAgent(ctx, caller, target)
	if lookupErr != nil || !launching {
		return err
	}
	return fmt.Errorf("%w; agent %q is still launching (launch_pending=true, pane_id=%q). Wait with agent_wait before prompting, or use pane_send_keys with that pane_id for raw terminal input", err, target, paneID)
}

func (s *Server) waitForAgentReady(ctx context.Context, caller herdr.Transport, target string, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	for {
		result, err := caller.Call(ctx, "agent.get", json.RawMessage(fmt.Sprintf(`{"target":%q}`, target)))
		if err == nil {
			var response struct {
				Agent map[string]any `json:"agent"`
			}
			if json.Unmarshal(result, &response) == nil && response.Agent != nil {
				if ready, _ := response.Agent["interactive_ready"].(bool); ready {
					return response.Agent, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for interactive readiness after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Server) launchingAgent(ctx context.Context, caller herdr.Transport, target string) (bool, string, error) {
	result, err := caller.Call(ctx, "agent.list", nil)
	if err != nil {
		return false, "", err
	}
	var response struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return false, "", err
	}
	for _, agent := range response.Agents {
		name, _ := agent["name"].(string)
		paneID, _ := agent["pane_id"].(string)
		if target != name && target != paneID {
			continue
		}
		launching, _ := agent["launch_pending"].(bool)
		return launching, paneID, nil
	}
	return false, "", nil
}

func stringArgument(arguments json.RawMessage, name string) string {
	var decoded map[string]any
	if json.Unmarshal(arguments, &decoded) != nil {
		return ""
	}
	value, _ := decoded[name].(string)
	return value
}

func argumentTimeout(arguments json.RawMessage, fallback time.Duration) time.Duration {
	var decoded map[string]any
	if json.Unmarshal(arguments, &decoded) != nil {
		return fallback
	}
	milliseconds, _ := decoded["timeout_ms"].(float64)
	if milliseconds <= 0 {
		return fallback
	}
	return time.Duration(milliseconds) * time.Millisecond
}
