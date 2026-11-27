// Package schema provides helpers for building tool input schemas as plain
// JSON Schema. No dependency on any LLM SDK — the produced struct serializes
// to the JSON Schema fragment all major providers (Anthropic, OpenAI, Google,
// MCP) accept.
package schema

import "encoding/json"

// InputSchema is a JSON Schema "object" with named properties. It marshals to
// the standard tool-input shape: {"type":"object","properties":{...},"required":[...]}.
type InputSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
	Required   []string       `json:"required,omitempty"`
}

// Parse unmarshals tool input JSON into the given type.
func Parse[T any](input string) (T, error) {
	var v T
	err := json.Unmarshal([]byte(input), &v)
	return v, err
}

// Props builds an InputSchema from required-field names and a property map.
// Pass nil for required to omit it.
func Props(required []string, properties map[string]any) InputSchema {
	return InputSchema{
		Type:       "object",
		Properties: properties,
		Required:   required,
	}
}

// Str creates a string property definition.
func Str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// Integer creates an integer property definition.
func Integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// Bool creates a boolean property definition.
func Bool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// Array creates an array property definition.
func Array(desc string, itemType string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": desc,
		"items":       map[string]string{"type": itemType},
	}
}
