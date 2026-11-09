package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"
)

// readLineMethods take a "lines" argument that bounds how much scrollback one
// call returns. pane.read is included because it reads the same buffers; a cap
// on agent_read alone would be one tool name away from no cap at all.
var readLineMethods = map[string]bool{
	"agent.read": true,
	"pane.read":  true,
}

// readLineLimits returns the default and maximum "lines" for method, or zeros
// when neither --read-default-lines nor --read-max-lines applies to it.
func (s *Server) readLineLimits(method string) (defaultLines, maxLines int) {
	if !readLineMethods[method] {
		return 0, 0
	}
	return s.readDefaultLines, s.readMaxLines
}

// withReadLineLimits returns a copy of input whose "lines" property states the
// bridge's default and maximum, so a caller sees them before the first call.
func withReadLineLimits(input map[string]any, defaultLines, maxLines int) map[string]any {
	properties, _ := input["properties"].(map[string]any)
	lines, ok := properties["lines"].(map[string]any)
	if !ok || (defaultLines <= 0 && maxLines <= 0) {
		return input
	}
	described := make(map[string]any, len(lines)+3)
	for key, value := range lines {
		described[key] = value
	}
	var parts []string
	if existing, _ := lines["description"].(string); existing != "" {
		parts = append(parts, existing)
	} else {
		parts = append(parts, "Number of lines to read.")
	}
	if defaultLines > 0 {
		described["default"] = defaultLines
		parts = append(parts, fmt.Sprintf("Defaults to %d when omitted.", defaultLines))
	}
	if maxLines > 0 {
		described["maximum"] = maxLines
		parts = append(parts, fmt.Sprintf("At most %d; larger values are capped.", maxLines))
	}
	described["description"] = strings.Join(parts, " ")

	mergedProperties := make(map[string]any, len(properties))
	for name, schema := range properties {
		mergedProperties[name] = schema
	}
	mergedProperties["lines"] = described
	clone := make(map[string]any, len(input))
	for key, value := range input {
		clone[key] = value
	}
	clone["properties"] = mergedProperties
	return clone
}

// limitReadLines fills in a missing "lines" with the default and caps a larger
// request at the maximum. The returned note tells the caller about a cap, so a
// shorter answer is not mistaken for the end of the scrollback.
func (s *Server) limitReadLines(method string, params json.RawMessage) (json.RawMessage, string) {
	defaultLines, maxLines := s.readLineLimits(method)
	if defaultLines <= 0 && maxLines <= 0 {
		return params, ""
	}
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil || decoded == nil {
		return params, ""
	}
	note := ""
	switch requested := decoded["lines"].(type) {
	case nil:
		if defaultLines <= 0 {
			return params, ""
		}
		decoded["lines"] = defaultLines
	case float64:
		if maxLines <= 0 || requested <= float64(maxLines) {
			return params, ""
		}
		decoded["lines"] = maxLines
		note = fmt.Sprintf("Capped lines at %d (requested %v): this bridge returns at most %d lines per read.", maxLines, requested, maxLines)
	default:
		// Not a number: leave it for Herdr to reject with its own message.
		return params, ""
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return params, ""
	}
	return encoded, note
}
