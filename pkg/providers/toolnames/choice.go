package toolnames

import (
	"encoding/json"
	"fmt"
	"github.com/vogler75/babel-gate/pkg/canonical"
)

// ParseChoice translates the two supported tool-selection wire representations.
func ParseChoice(value any, anthropic bool) (*canonical.ToolChoice, error) {
	if value == nil {
		return nil, nil
	}
	if mode, ok := value.(string); ok && !anthropic {
		switch mode {
		case "auto", "none", "required":
			return &canonical.ToolChoice{Mode: mode}, nil
		}
		return nil, fmt.Errorf("invalid tool choice %q", mode)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var wire struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err = json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	if anthropic {
		switch wire.Type {
		case "auto", "none":
			return &canonical.ToolChoice{Mode: wire.Type}, nil
		case "any":
			return &canonical.ToolChoice{Mode: "required"}, nil
		case "tool":
			if wire.Name != "" {
				return &canonical.ToolChoice{Mode: "named", Name: wire.Name}, nil
			}
		}
	} else if wire.Type == "function" && wire.Function.Name != "" {
		return &canonical.ToolChoice{Mode: "named", Name: wire.Function.Name}, nil
	}
	return nil, fmt.Errorf("invalid tool choice")
}
func WireChoice(choice *canonical.ToolChoice, anthropic bool) any {
	if choice == nil {
		return nil
	}
	if anthropic {
		mode := choice.Mode
		if mode == "required" {
			mode = "any"
		}
		if mode == "named" {
			return map[string]any{"type": "tool", "name": choice.Name}
		}
		return map[string]any{"type": mode}
	}
	if choice.Mode == "named" {
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
	}
	return choice.Mode
}
