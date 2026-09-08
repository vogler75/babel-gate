package openai

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/vogler75/babel-gate/pkg/providers/toolnames"
	"strings"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// ToOpenAIRequest converts a CanonicalRequest into an OpenAI ChatCompletionRequest.
func ToOpenAIRequest(req *canonical.CanonicalRequest) (*ChatCompletionRequest, error) {
	req, names := toolnames.Normalize(req, toolnames.Constraints{MaxLength: 64, Allowed: toolnames.ASCII})
	out := &ChatCompletionRequest{
		ToolChoice:  toolnames.WireChoice(req.ToolChoice, false),
		names:       names,
		Model:       req.Model,
		Temperature: req.Params.Temperature,
		TopP:        req.Params.TopP,
		Stop:        req.Params.Stop,
		Stream:      req.Stream,
	}

	modelLower := strings.ToLower(req.Model)
	isNewerModel := strings.HasPrefix(modelLower, "gpt-5") ||
		strings.HasPrefix(modelLower, "o1") ||
		strings.HasPrefix(modelLower, "o3") ||
		strings.HasPrefix(modelLower, "o4") ||
		strings.Contains(modelLower, "codex")

	if isNewerModel {
		out.MaxCompletionTokens = req.Params.MaxTokens
		if strings.HasPrefix(modelLower, "o1") || strings.HasPrefix(modelLower, "o3") {
			out.Temperature = nil
			out.TopP = nil
		}
	} else {
		out.MaxTokens = req.Params.MaxTokens
	}

	if req.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// Tools
	if len(req.Tools) > 0 {
		out.Tools = make([]ToolDefinition, 0, len(req.Tools))
		for _, t := range req.Tools {
			out.Tools = append(out.Tools, ToolDefinition{
				Type: "function",
				Function: FunctionDefinition{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			})
		}
	}

	// Messages
	for _, m := range req.Messages {
		switch m.Role {
		case canonical.RoleSystem:
			out.Messages = append(out.Messages, ChatMessage{
				Role:    "system",
				Content: m.TextContent(),
			})

		case canonical.RoleUser:
			var parts []ContentPart
			for _, p := range m.Parts {
				switch p.Type {
				case canonical.PartText:
					parts = append(parts, ContentPart{
						Type: "text",
						Text: p.Text,
					})
				case canonical.PartImage:
					url := p.ImageURL
					if url == "" && p.ImageData != "" {
						mime := p.ImageMediaType
						if mime == "" {
							mime = "image/png"
						}
						url = fmt.Sprintf("data:%s;base64,%s", mime, p.ImageData)
					}
					parts = append(parts, ContentPart{
						Type:     "image_url",
						ImageURL: &ImageURL{URL: url},
					})
				}
			}
			if len(parts) == 1 && parts[0].Type == "text" {
				out.Messages = append(out.Messages, ChatMessage{
					Role:    "user",
					Content: parts[0].Text,
				})
			} else if len(parts) > 0 {
				out.Messages = append(out.Messages, ChatMessage{
					Role:    "user",
					Content: parts,
				})
			}

		case canonical.RoleAssistant:
			var text string
			var toolCalls []ToolCall
			for _, p := range m.Parts {
				switch p.Type {
				case canonical.PartText:
					text += p.Text
				case canonical.PartToolCall:
					toolCalls = append(toolCalls, ToolCall{
						ID:   p.ToolCallID,
						Type: "function",
						Function: FunctionCallInfo{
							Name:      p.ToolCallName,
							Arguments: p.ToolCallArgs,
						},
					})
				}
			}
			msg := ChatMessage{
				Role:      "assistant",
				Content:   text,
				ToolCalls: toolCalls,
			}
			out.Messages = append(out.Messages, msg)

		case canonical.RoleTool:
			for _, p := range m.Parts {
				if p.Type == canonical.PartToolResult {
					out.Messages = append(out.Messages, ChatMessage{
						Role:       "tool",
						ToolCallID: p.ToolResultID,
						Content:    p.ToolResultContent,
					})
				}
			}
		}
	}

	return out, nil
}

// FromOpenAIResponse converts an OpenAI ChatCompletionResponse into a CanonicalResponse.
func FromOpenAIResponse(resp *ChatCompletionResponse) (*canonical.CanonicalResponse, error) {
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned by OpenAI")
	}

	choice := resp.Choices[0]
	msg := canonical.Message{
		Role: canonical.RoleAssistant,
	}

	if choice.Message.Content != nil {
		switch c := choice.Message.Content.(type) {
		case string:
			if c != "" {
				msg.Parts = append(msg.Parts, canonical.ContentPart{
					Type: canonical.PartText,
					Text: c,
				})
			}
		}
	}

	for _, tc := range choice.Message.ToolCalls {
		msg.Parts = append(msg.Parts, canonical.ContentPart{
			Type:         canonical.PartToolCall,
			ToolCallID:   tc.ID,
			ToolCallName: tc.Function.Name,
			ToolCallArgs: tc.Function.Arguments,
		})
	}

	canonicalResp := &canonical.CanonicalResponse{
		ID:           resp.ID,
		Model:        resp.Model,
		Message:      msg,
		FinishReason: choice.FinishReason,
	}

	if resp.Usage != nil {
		canonicalResp.Usage = canonical.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}

	return canonicalResp, nil
}

// ParseOpenAIStreamEvent converts an OpenAI StreamChunk into canonical events.
func ParseOpenAIStreamEvent(chunk *StreamChunk) []canonical.CanonicalEvent {
	var events []canonical.CanonicalEvent

	if chunk.Usage != nil {
		events = append(events, canonical.CanonicalEvent{
			Type:      canonical.EventMessageDelta,
			MessageID: chunk.ID,
			Model:     chunk.Model,
			Usage: &canonical.Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			},
		})
	}

	for _, choice := range chunk.Choices {
		firstEvent := len(events)
		if choice.Delta.Role != "" {
			events = append(events, canonical.CanonicalEvent{
				Type:      canonical.EventMessageStart,
				MessageID: chunk.ID,
				Model:     chunk.Model,
			})
		}

		// Prefer reasoning_content when both compatible-provider aliases occur.
		thinking := choice.Delta.ReasoningContent
		if thinking == "" {
			thinking = choice.Delta.Reasoning
		}
		if thinking != "" {
			events = append(events, canonical.CanonicalEvent{Type: canonical.EventThinkingDelta, MessageID: chunk.ID, Thinking: thinking})
		}
		if choice.Delta.Content != "" {
			events = append(events, canonical.CanonicalEvent{
				Type:      canonical.EventTextDelta,
				MessageID: chunk.ID,
				Index:     choice.Index,
				Text:      choice.Delta.Content,
			})
		}

		for _, tc := range choice.Delta.ToolCalls {
			tcIndex := 0
			if tc.Index != nil {
				tcIndex = *tc.Index
			}
			if tc.ID != "" || tc.Function.Name != "" {
				events = append(events, canonical.CanonicalEvent{
					Type:         canonical.EventToolCallStart,
					MessageID:    chunk.ID,
					Index:        tcIndex,
					ToolCallID:   tc.ID,
					ToolCallName: tc.Function.Name,
				})
			}
			if tc.Function.Arguments != "" {
				events = append(events, canonical.CanonicalEvent{
					Type:         canonical.EventToolCallDelta,
					MessageID:    chunk.ID,
					Index:        tcIndex,
					ToolCallID:   tc.ID,
					ToolCallArgs: tc.Function.Arguments,
				})
			}
		}

		if choice.FinishReason != "" {
			events = append(events, canonical.CanonicalEvent{
				Type:         canonical.EventMessageDelta,
				MessageID:    chunk.ID,
				Index:        choice.Index,
				FinishReason: choice.FinishReason,
			})
		}
		for i := firstEvent; i < len(events); i++ {
			events[i].CandidateIndex = choice.Index
		}
	}

	return events
}

// Helper to unmarshal raw line
func UnmarshalStreamChunk(data []byte) (*StreamChunk, error) {
	var envelope struct {
		// Azure-compatible gateways can emit prompt-filter metadata before choices.
		PromptFilterResults []json.RawMessage `json:"prompt_filter_results"`
		PromptAnnotations   []json.RawMessage `json:"prompt_annotations"`

		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Delta json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("invalid completion chunk: %w", err)
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return nil, fmt.Errorf("upstream stream error: %s", envelope.Error)
	}
	var chunk StreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if len(chunk.Choices) == 0 && chunk.Usage == nil && len(envelope.PromptFilterResults) == 0 && len(envelope.PromptAnnotations) == 0 {
		return nil, fmt.Errorf("completion chunk has neither choices nor usage")
	}
	for _, choice := range envelope.Choices {
		if len(choice.Delta) == 0 || choice.Delta[0] != '{' {
			return nil, fmt.Errorf("completion choice has no delta object")
		}
	}
	return &chunk, nil
}

// FromOpenAIRequest converts an incoming OpenAI ChatCompletionRequest into a CanonicalRequest.
func FromOpenAIRequest(req *ChatCompletionRequest) (*canonical.CanonicalRequest, error) {
	choice, err := toolnames.ParseChoice(req.ToolChoice, false)
	if err != nil {
		return nil, err
	}
	out := &canonical.CanonicalRequest{
		ToolChoice: choice,
		Model:      req.Model,
		Stream:     req.Stream,
		Params: canonical.Parameters{
			Temperature: req.Temperature,
			TopP:        req.TopP,
			MaxTokens:   req.MaxTokens,
			Stop:        req.Stop,
		},
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, canonical.ToolDeclaration{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			var text string
			if str, ok := m.Content.(string); ok {
				text = str
			}
			out.Messages = append(out.Messages, canonical.Message{
				Role: canonical.RoleSystem,
				Parts: []canonical.ContentPart{
					{Type: canonical.PartText, Text: text},
				},
			})

		case "user":
			var parts []canonical.ContentPart
			switch c := m.Content.(type) {
			case string:
				parts = append(parts, canonical.ContentPart{Type: canonical.PartText, Text: c})
			case []any:
				for _, raw := range c {
					if itemMap, ok := raw.(map[string]any); ok {
						itemType, _ := itemMap["type"].(string)
						if itemType == "text" {
							if txt, ok := itemMap["text"].(string); ok {
								parts = append(parts, canonical.ContentPart{Type: canonical.PartText, Text: txt})
							}
						} else if itemType == "image_url" {
							if imgMap, ok := itemMap["image_url"].(map[string]any); ok {
								url, _ := imgMap["url"].(string)
								parts = append(parts, canonical.ContentPart{Type: canonical.PartImage, ImageURL: url})
							}
						}
					}
				}
			}
			out.Messages = append(out.Messages, canonical.Message{
				Role:  canonical.RoleUser,
				Parts: parts,
			})

		case "assistant":
			var parts []canonical.ContentPart
			if str, ok := m.Content.(string); ok && str != "" {
				parts = append(parts, canonical.ContentPart{Type: canonical.PartText, Text: str})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, canonical.ContentPart{
					Type:         canonical.PartToolCall,
					ToolCallID:   tc.ID,
					ToolCallName: tc.Function.Name,
					ToolCallArgs: tc.Function.Arguments,
				})
			}
			out.Messages = append(out.Messages, canonical.Message{
				Role:  canonical.RoleAssistant,
				Parts: parts,
			})

		case "tool":
			contentStr := ""
			if str, ok := m.Content.(string); ok {
				contentStr = str
			} else if m.Content != nil {
				b, _ := json.Marshal(m.Content)
				contentStr = string(b)
			}
			out.Messages = append(out.Messages, canonical.Message{
				Role: canonical.RoleTool,
				Parts: []canonical.ContentPart{
					{
						Type:              canonical.PartToolResult,
						ToolResultID:      m.ToolCallID,
						ToolResultContent: contentStr,
					},
				},
			})
		}
	}

	return out, nil
}

// ToOpenAIResponse converts a CanonicalResponse into an OpenAI ChatCompletionResponse.
func ToOpenAIResponse(resp *canonical.CanonicalResponse) (*ChatCompletionResponse, error) {
	id := resp.ID
	if id == "" {
		id = "chatcmpl-" + rand.Text()
	}

	finishReason := resp.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}

	msg := ChatMessage{
		Role: "assistant",
	}

	var text string
	var toolCalls []ToolCall
	for i, p := range resp.Message.Parts {
		switch p.Type {
		case canonical.PartText:
			text += p.Text
		case canonical.PartToolCall:
			idx := i
			toolCalls = append(toolCalls, ToolCall{
				Index: &idx,
				ID:    p.ToolCallID,
				Type:  "function",
				Function: FunctionCallInfo{
					Name:      p.ToolCallName,
					Arguments: p.ToolCallArgs,
				},
			})
		}
	}

	if text != "" {
		msg.Content = text
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
		if finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}

	out := &ChatCompletionResponse{
		ID:    id,
		Model: resp.Model,
		Choices: []Choice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
		Usage: &Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}

	return out, nil
}
