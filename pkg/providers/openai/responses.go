package openai

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/toolnames"
)

// ResponsesRequest is the subset of the OpenAI Responses API request that can
// be represented by BabelGate's provider-neutral canonical request.
type ResponsesRequest struct {
	toolInfo        map[string]ResponsesToolInfo
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    json.RawMessage `json:"instructions,omitempty"`
	Tools           []ResponsesTool `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Reasoning       *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
}

type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  map[string]any  `json:"parameters,omitempty"`
	Tools       []ResponsesTool `json:"tools,omitempty"`
	Format      json.RawMessage `json:"format,omitempty"`
}

// ResponsesToolInfo retains Responses-only tool properties across the
// provider-neutral canonical layer.
type ResponsesToolInfo struct {
	Kind      string
	Namespace string
	Name      string
}

type responsesInputItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Tools     []ResponsesTool `json:"tools,omitempty"`
}

type responsesContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

// FromResponsesRequest converts an OpenAI Responses API request into the
// canonical conversation used by the router.
func FromResponsesRequest(req *ResponsesRequest) (*canonical.CanonicalRequest, error) {
	choice, err := parseResponsesToolChoice(req.ToolChoice)
	if err != nil {
		return nil, err
	}
	out := &canonical.CanonicalRequest{
		Model:      req.Model,
		Stream:     req.Stream,
		ToolChoice: choice,
		Params: canonical.Parameters{
			Temperature: req.Temperature,
			TopP:        req.TopP,
			MaxTokens:   req.MaxOutputTokens,
		},
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out.Thinking = &canonical.ThinkingConfig{Type: "enabled", Level: req.Reasoning.Effort}
	}

	if len(req.Instructions) > 0 && string(req.Instructions) != "null" {
		text, err := responsesText(req.Instructions)
		if err != nil {
			return nil, fmt.Errorf("invalid instructions: %w", err)
		}
		if text != "" {
			out.Messages = append(out.Messages, canonical.Message{Role: canonical.RoleSystem, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: text}}})
		}
	}

	req.toolInfo = make(map[string]ResponsesToolInfo)
	for _, tool := range req.Tools {
		if err := req.addTool(out, tool, "", ""); err != nil {
			return nil, err
		}
	}

	if len(req.Input) == 0 || string(req.Input) == "null" {
		return nil, fmt.Errorf("input is required")
	}
	var prompt string
	if err := json.Unmarshal(req.Input, &prompt); err == nil {
		out.Messages = append(out.Messages, canonical.Message{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: prompt}}})
		return out, nil
	}

	var items []responsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or an array of input items: %w", err)
	}
	appendPart := func(role string, part canonical.ContentPart) {
		if len(out.Messages) > 0 && out.Messages[len(out.Messages)-1].Role == role {
			out.Messages[len(out.Messages)-1].Parts = append(out.Messages[len(out.Messages)-1].Parts, part)
			return
		}
		out.Messages = append(out.Messages, canonical.Message{Role: role, Parts: []canonical.ContentPart{part}})
	}
	for _, item := range items {
		switch item.Type {
		case "additional_tools":
			for _, tool := range item.Tools {
				if err := req.addTool(out, tool, "", ""); err != nil {
					return nil, fmt.Errorf("invalid additional_tools item: %w", err)
				}
			}
		case "function_call":
			name := req.canonicalToolName(item.Namespace, item.Name)
			appendPart(canonical.RoleAssistant, canonical.ContentPart{
				Type: canonical.PartToolCall, ToolCallID: item.CallID, ToolCallName: name, ToolCallArgs: item.Arguments,
			})
		case "custom_tool_call":
			name := req.canonicalToolName(item.Namespace, item.Name)
			input, err := responsesText(item.Input)
			if err != nil {
				return nil, fmt.Errorf("invalid custom_tool_call input: %w", err)
			}
			args, _ := json.Marshal(map[string]any{"input": input})
			appendPart(canonical.RoleAssistant, canonical.ContentPart{
				Type: canonical.PartToolCall, ToolCallID: item.CallID, ToolCallName: name, ToolCallArgs: string(args),
			})
		case "function_call_output", "custom_tool_call_output":
			result, err := responsesToolResult(item.CallID, item.Output)
			if err != nil {
				return nil, fmt.Errorf("invalid function_call_output: %w", err)
			}
			appendPart(canonical.RoleTool, result)
		case "message", "":
			role := item.Role
			switch role {
			case "developer", "system":
				role = canonical.RoleSystem
			case "user":
				role = canonical.RoleUser
			case "assistant":
				role = canonical.RoleAssistant
			default:
				return nil, fmt.Errorf("unsupported input message role %q", item.Role)
			}
			parts, err := responsesParts(item.Content)
			if err != nil {
				return nil, err
			}
			out.Messages = append(out.Messages, canonical.Message{Role: role, Parts: parts})
		case "reasoning", "compaction", "compaction_trigger", "item_reference":
			// These Responses state-management items have no portable prompt
			// representation. The surrounding messages retain the usable context.
		default:
			return nil, fmt.Errorf("unsupported Responses API input item type %q", item.Type)
		}
	}
	return out, nil
}

func (req *ResponsesRequest) canonicalToolName(namespace, name string) string {
	if namespace == "" || namespace == "functions" {
		return name
	}
	return namespace + "__" + name
}

func (req *ResponsesRequest) addTool(out *canonical.CanonicalRequest, tool ResponsesTool, namespace, namespaceDescription string) error {
	if tool.Type == "namespace" {
		if tool.Name == "" {
			return fmt.Errorf("tool namespace name is required")
		}
		for _, nested := range tool.Tools {
			if err := req.addTool(out, nested, tool.Name, tool.Description); err != nil {
				return err
			}
		}
		return nil
	}
	if tool.Type != "function" && tool.Type != "custom" {
		return fmt.Errorf("unsupported Responses API tool type %q", tool.Type)
	}
	if tool.Name == "" {
		return fmt.Errorf("%s tool name is required", tool.Type)
	}
	canonicalName := req.canonicalToolName(namespace, tool.Name)
	if previous, exists := req.toolInfo[canonicalName]; exists {
		if previous.Name != tool.Name || previous.Namespace != namespace || previous.Kind != tool.Type {
			return fmt.Errorf("tool name collision after flattening namespace: %q", canonicalName)
		}
		return nil
	}
	parameters := tool.Parameters
	description := tool.Description
	if namespaceDescription != "" {
		description = namespaceDescription + "\n\n" + description
	}
	if tool.Type == "custom" {
		inputDescription := "Free-form input for this tool."
		if len(tool.Format) > 0 && string(tool.Format) != "null" {
			inputDescription += " Expected format: " + string(tool.Format)
		}
		parameters = map[string]any{
			"type":       "object",
			"properties": map[string]any{"input": map[string]any{"type": "string", "description": inputDescription}},
			"required":   []string{"input"}, "additionalProperties": false,
		}
	}
	req.toolInfo[canonicalName] = ResponsesToolInfo{Kind: tool.Type, Namespace: namespace, Name: tool.Name}
	out.Tools = append(out.Tools, canonical.ToolDeclaration{Name: canonicalName, Description: description, Parameters: parameters})
	return nil
}

// ToolInfo returns the original Responses API kind and namespace for a
// canonicalized tool name.
func (req *ResponsesRequest) ToolInfo(canonicalName string) (ResponsesToolInfo, bool) {
	if req == nil {
		return ResponsesToolInfo{}, false
	}
	info, ok := req.toolInfo[canonicalName]
	return info, ok
}

func responsesToolResult(callID string, raw json.RawMessage) (canonical.ContentPart, error) {
	result := canonical.ContentPart{Type: canonical.PartToolResult, ToolResultID: callID}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		result.ToolResultContent = text
		return result, nil
	}
	if parts, err := responsesParts(raw); err == nil {
		result.ToolResultParts = parts
		result.ToolResultContent = result.ToolResultText()
		return result, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return result, err
	}
	b, _ := json.Marshal(value)
	result.ToolResultContent = string(b)
	return result, nil
}

func parseResponsesToolChoice(value any) (*canonical.ToolChoice, error) {
	if value == nil {
		return nil, nil
	}
	if mode, ok := value.(string); ok {
		return toolnames.ParseChoice(mode, false)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var wire struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return nil, err
	}
	if wire.Type == "function" && wire.Name != "" {
		return &canonical.ToolChoice{Mode: "named", Name: wire.Name}, nil
	}
	return nil, fmt.Errorf("invalid tool choice")
}

func responsesParts(raw json.RawMessage) ([]canonical.ContentPart, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []canonical.ContentPart{{Type: canonical.PartText, Text: text}}, nil
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("message content must be a string or array: %w", err)
	}
	out := make([]canonical.ContentPart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			out = append(out, canonical.ContentPart{Type: canonical.PartText, Text: part.Text})
		case "input_image", "image_url":
			var url string
			if err := json.Unmarshal(part.ImageURL, &url); err != nil || url == "" {
				return nil, fmt.Errorf("input_image requires an image_url string")
			}
			out = append(out, canonical.ContentPart{Type: canonical.PartImage, ImageURL: url})
		default:
			return nil, fmt.Errorf("unsupported Responses API content type %q", part.Type)
		}
	}
	return out, nil
}

func responsesText(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	parts, err := responsesParts(raw)
	if err == nil {
		var texts []string
		for _, part := range parts {
			if part.Type == canonical.PartText {
				texts = append(texts, part.Text)
			}
		}
		return strings.Join(texts, ""), nil
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		b, _ := json.Marshal(value)
		return string(b), nil
	}
	return "", err
}

// ToResponsesResponse converts a completed canonical response to the native
// OpenAI Responses API envelope.
func ToResponsesResponse(resp *canonical.CanonicalResponse, requestedModel string, request ...*ResponsesRequest) map[string]any {
	id := resp.ID
	if !strings.HasPrefix(id, "resp_") {
		id = "resp_" + rand.Text()
	}
	model := resp.Model
	if model == "" {
		model = requestedModel
	}
	status := "completed"
	var incomplete any
	if resp.FinishReason == "length" || resp.FinishReason == "max_tokens" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}

	output := make([]any, 0, len(resp.Message.Parts))
	var text strings.Builder
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + rand.Text(), "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}, "logprobs": []any{}}},
		})
		text.Reset()
	}
	for _, part := range resp.Message.Parts {
		switch part.Type {
		case canonical.PartText:
			text.WriteString(part.Text)
		case canonical.PartThinking:
			flushText()
			output = append(output, map[string]any{
				"type": "reasoning", "id": "rs_" + rand.Text(), "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": part.Thinking}},
			})
		case canonical.PartToolCall:
			flushText()
			callID := part.ToolCallID
			if callID == "" {
				callID = "call_" + rand.Text()
			}
			var wireReq *ResponsesRequest
			if len(request) > 0 {
				wireReq = request[0]
			}
			output = append(output, ResponsesToolCallItem(part.ToolCallName, callID, part.ToolCallArgs, "completed", wireReq))
		}
	}
	flushText()

	now := time.Now().Unix()
	return map[string]any{
		"id": id, "object": "response", "created_at": now, "completed_at": now,
		"status": status, "error": nil, "incomplete_details": incomplete,
		"model": model, "output": output, "parallel_tool_calls": true,
		"usage": map[string]any{
			"input_tokens":          resp.Usage.PromptTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": resp.Usage.CacheReadInputTokens},
			"output_tokens":         resp.Usage.CompletionTokens,
			"output_tokens_details": map[string]any{"reasoning_tokens": resp.Usage.ReasoningTokens},
			"total_tokens":          resp.Usage.TotalTokens,
		},
	}
}

// ResponsesToolCallItem restores Responses-only custom/namespace metadata on
// a tool call that has crossed the canonical layer.
func ResponsesToolCallItem(canonicalName, callID, arguments, status string, req *ResponsesRequest) map[string]any {
	info, ok := req.ToolInfo(canonicalName)
	if !ok {
		info = ResponsesToolInfo{Kind: "function", Name: canonicalName}
	}
	prefix := "fc_"
	item := map[string]any{
		"type": "function_call", "id": prefix + rand.Text(), "status": status,
		"call_id": callID, "name": info.Name, "arguments": arguments,
	}
	if info.Namespace != "" {
		item["namespace"] = info.Namespace
	}
	if info.Kind == "custom" {
		item["type"] = "custom_tool_call"
		item["id"] = "ctc_" + rand.Text()
		item["input"] = ResponsesCustomToolInput(arguments)
		delete(item, "arguments")
	}
	return item
}

func ResponsesCustomToolInput(arguments string) string {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if raw, ok := value["input"]; ok {
			var input string
			if json.Unmarshal(raw, &input) == nil {
				return input
			}
		}
	}
	return arguments
}
