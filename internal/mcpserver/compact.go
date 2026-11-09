package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
)

// compactResult shrinks a Herdr result for --compact-results: agent.list keeps
// only the fields a caller needs to pick an agent, and every other result is
// re-encoded without insignificant whitespace. An agent.list result of an
// unexpected shape is only stripped of whitespace, and anything that is not
// valid JSON is returned untouched, rather than failing the call.
func compactResult(method string, result json.RawMessage) json.RawMessage {
	if method == "agent.list" {
		if compacted, err := compactAgentList(result); err == nil {
			return compacted
		}
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, result); err != nil {
		return result
	}
	return buffer.Bytes()
}

// compactAgent is one agent.list entry reduced to what tells agents apart.
// agent_get still returns the full record.
type compactAgent struct {
	PaneID  string `json:"pane_id"`
	Name    string `json:"name,omitempty"`
	Status  string `json:"status"`
	Repo    string `json:"repo,omitempty"`
	Title   string `json:"title,omitempty"`
	Context any    `json:"context,omitempty"`
	Focused bool   `json:"focused,omitempty"`
}

// compactAgentList replaces Herdr's 27 fields per agent with the seven in
// compactAgent and adds a per-status count on top.
func compactAgentList(result json.RawMessage) (json.RawMessage, error) {
	var full struct {
		Type   string `json:"type"`
		Agents []struct {
			PaneID  string         `json:"pane_id"`
			Name    string         `json:"name"`
			Status  string         `json:"agent_status"`
			Cwd     string         `json:"cwd"`
			Title   string         `json:"terminal_title_stripped"`
			Tokens  map[string]any `json:"tokens"`
			Focused bool           `json:"focused"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(result, &full); err != nil {
		return nil, err
	}
	if full.Agents == nil {
		return nil, fmt.Errorf("agent.list result has no agents array")
	}
	summary := make(map[string]int)
	agents := make([]compactAgent, 0, len(full.Agents))
	for _, agent := range full.Agents {
		summary[agent.Status]++
		entry := compactAgent{
			PaneID:  agent.PaneID,
			Name:    agent.Name,
			Status:  agent.Status,
			Title:   agent.Title,
			Context: agent.Tokens["context"],
			Focused: agent.Focused,
		}
		if agent.Cwd != "" {
			entry.Repo = path.Base(agent.Cwd)
		}
		agents = append(agents, entry)
	}
	return json.Marshal(struct {
		Type    string         `json:"type,omitempty"`
		Summary map[string]int `json:"summary"`
		Agents  []compactAgent `json:"agents"`
	}{full.Type, summary, agents})
}
