package anthropic

import (
	"bytes"
	"encoding/json"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/toolnames"
)

// NormalizePayload changes only tool names, preserving unknown Anthropic fields
// and signed thinking blocks on the direct passthrough path.
func NormalizePayload(data []byte) ([]byte, *toolnames.Mapping, error) {
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, nil, err
	}
	req := &canonical.CanonicalRequest{}
	var fields []map[string]any
	collect := func(value any) {
		if field, ok := value.(map[string]any); ok {
			if name, ok := field["name"].(string); ok {
				req.Tools = append(req.Tools, canonical.ToolDeclaration{Name: name})
				fields = append(fields, field)
			}
		}
	}
	if tools, ok := raw["tools"].([]any); ok {
		for _, tool := range tools {
			collect(tool)
		}
	}
	if messages, ok := raw["messages"].([]any); ok {
		for _, message := range messages {
			if m, ok := message.(map[string]any); ok {
				if content, ok := m["content"].([]any); ok {
					for _, block := range content {
						if b, ok := block.(map[string]any); ok && b["type"] == "tool_use" {
							collect(b)
						}
					}
				}
			}
		}
	}
	if choice, ok := raw["tool_choice"].(map[string]any); ok && choice["type"] == "tool" {
		collect(choice)
	}
	_, names := toolnames.Normalize(req, toolnames.Constraints{MaxLength: 64, Allowed: toolnames.ASCII})
	if !names.Changed() {
		return data, names, nil
	}
	for _, field := range fields {
		field["name"] = names.Name(field["name"].(string))
	}
	encoded, err := json.Marshal(raw)
	return encoded, names, err
}

// RestorePayload handles either a full message or a content_block_start event.
func RestorePayload(data []byte, names *toolnames.Mapping) []byte {
	if !names.Changed() {
		return data
	}
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&raw) != nil {
		return data
	}
	restore := func(value any) {
		if block, ok := value.(map[string]any); ok && block["type"] == "tool_use" {
			if name, ok := block["name"].(string); ok {
				block["name"] = names.Original(name)
			}
		}
	}
	restore(raw["content_block"])
	if content, ok := raw["content"].([]any); ok {
		for _, block := range content {
			restore(block)
		}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return data
	}
	return encoded
}
