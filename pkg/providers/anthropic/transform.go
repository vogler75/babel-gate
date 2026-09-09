package anthropic

import (
	"encoding/json"
	"github.com/vogler75/babel-gate/pkg/providers/toolnames"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// ToAnthropicRequest converts a CanonicalRequest into an Anthropic MessageRequest.
func ToAnthropicRequest(req *canonical.CanonicalRequest) (*MessageRequest, error) {
	req, names := toolnames.Normalize(req, toolnames.Constraints{MaxLength: 64, Allowed: toolnames.ASCII})
	maxTokens := 4096
	if req.Params.MaxTokens != nil && *req.Params.MaxTokens > 0 {
		maxTokens = *req.Params.MaxTokens
	}

	out := &MessageRequest{
		ToolChoice:    toolnames.WireChoice(req.ToolChoice, true),
		names:         names,
		Model:         req.Model,
		MaxTokens:     maxTokens,
		Temperature:   req.Params.Temperature,
		TopP:          req.Params.TopP,
		TopK:          req.Params.TopK,
		StopSequences: req.Params.Stop,
		Stream:        req.Stream,
	}

	// System prompt
	if sys := req.SystemPrompt(); sys != "" {
		out.System = sys
	}

	// Tools
	if len(req.Tools) > 0 {
		out.Tools = make([]ToolDefinition, 0, len(req.Tools))
		for _, t := range req.Tools {
			out.Tools = append(out.Tools, ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.Parameters,
			})
		}
	}

	// Messages
	for _, m := range req.NonSystemMessages() {
		anthropicRole := "user"
		if m.Role == canonical.RoleAssistant {
			anthropicRole = "assistant"
		}

		var blocks []ContentBlock
		for _, p := range m.Parts {
			switch p.Type {
			case canonical.PartText:
				blocks = append(blocks, ContentBlock{
					Type: "text",
					Text: p.Text,
				})

			case canonical.PartThinking:
				blocks = append(blocks, ContentBlock{
					Type:      "thinking",
					Thinking:  p.Thinking,
					Signature: p.ThoughtSignature,
				})

			case canonical.PartImage:
				if p.ImageData != "" {
					mediaType := p.ImageMediaType
					if mediaType == "" {
						mediaType = "image/png"
					}
					blocks = append(blocks, ContentBlock{
						Type: "image",
						Source: &ImageSource{
							Type:      "base64",
							MediaType: mediaType,
							Data:      p.ImageData,
						},
					})
				}

			case canonical.PartToolCall:
				var inputMap map[string]any
				if p.ToolCallArgs != "" {
					_ = json.Unmarshal([]byte(p.ToolCallArgs), &inputMap)
				}
				if inputMap == nil {
					inputMap = make(map[string]any)
				}
				blocks = append(blocks, ContentBlock{
					Type:  "tool_use",
					ID:    p.ToolCallID,
					Name:  p.ToolCallName,
					Input: inputMap,
				})

			case canonical.PartToolResult:
				var content any = p.ToolResultContent
				if len(p.ToolResultParts) > 0 {
					var resultBlocks []ContentBlock
					for _, part := range p.ToolResultParts {
						switch part.Type {
						case canonical.PartText:
							resultBlocks = append(resultBlocks, ContentBlock{Type: "text", Text: part.Text})
						case canonical.PartImage:
							resultBlocks = append(resultBlocks, ContentBlock{Type: "image", Source: &ImageSource{Type: "base64", MediaType: part.ImageMediaType, Data: part.ImageData}})
						}
					}
					content = resultBlocks
				}
				blocks = append(blocks, ContentBlock{
					Type:      "tool_result",
					ToolUseID: p.ToolResultID,
					Content:   content,
					IsError:   p.ToolResultError,
				})
			}
		}

		if len(blocks) == 1 && blocks[0].Type == "text" {
			out.Messages = append(out.Messages, Message{
				Role:    anthropicRole,
				Content: blocks[0].Text,
			})
		} else if len(blocks) > 0 {
			out.Messages = append(out.Messages, Message{
				Role:    anthropicRole,
				Content: blocks,
			})
		}
	}

	return out, nil
}

// FromAnthropicResponse converts an Anthropic MessageResponse into a CanonicalResponse.
func FromAnthropicResponse(resp *MessageResponse) (*canonical.CanonicalResponse, error) {
	msg := canonical.Message{
		Role: canonical.RoleAssistant,
	}

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			msg.Parts = append(msg.Parts, canonical.ContentPart{
				Type: canonical.PartText,
				Text: block.Text,
			})

		case "thinking":
			msg.Parts = append(msg.Parts, canonical.ContentPart{
				Type:     canonical.PartThinking,
				Thinking: block.Thinking,
			})

		case "tool_use":
			var argsJSON string
			if block.Input != nil {
				if b, err := json.Marshal(block.Input); err == nil {
					argsJSON = string(b)
				}
			}
			msg.Parts = append(msg.Parts, canonical.ContentPart{
				Type:         canonical.PartToolCall,
				ToolCallID:   block.ID,
				ToolCallName: block.Name,
				ToolCallArgs: argsJSON,
			})
		}
	}

	finishReason := "stop"
	if resp.StopReason == "tool_use" {
		finishReason = "tool_calls"
	} else if resp.StopReason == "max_tokens" {
		finishReason = "length"
	}

	return &canonical.CanonicalResponse{
		ID:           resp.ID,
		Model:        resp.Model,
		Message:      msg,
		FinishReason: finishReason,
		Usage:        resp.Usage.Canonical(),
	}, nil
}

// ParseAnthropicStreamEvent converts Anthropic SSE line into canonical events.
func ParseAnthropicStreamEvent(data []byte) ([]canonical.CanonicalEvent, error) {
	return parseAnthropicStreamEvent(data, &Usage{})
}

func parseAnthropicStreamEvent(data []byte, accumulated *Usage) ([]canonical.CanonicalEvent, error) {
	var event StreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	// Usage deltas omit unchanged counters. Decode into the existing value so
	// final fresh-input corrections preserve cache reads/writes from the start.
	var envelope struct {
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	for _, raw := range []json.RawMessage{envelope.Message.Usage, envelope.Usage} {
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, accumulated); err != nil {
				return nil, err
			}
		}
	}

	var out []canonical.CanonicalEvent

	switch event.Type {
	case "message_start":
		if event.Message != nil {
			out = append(out, canonical.CanonicalEvent{
				Type:      canonical.EventMessageStart,
				MessageID: event.Message.ID,
				Model:     event.Message.Model,
				Usage:     usagePointer(accumulated.Canonical()),
			})
		}

	case "content_block_start":
		if event.ContentBlock != nil {
			if event.ContentBlock.Type == "tool_use" {
				out = append(out, canonical.CanonicalEvent{
					Type:         canonical.EventToolCallStart,
					Index:        event.Index,
					ToolCallID:   event.ContentBlock.ID,
					ToolCallName: event.ContentBlock.Name,
				})
			}
		}

	case "content_block_delta":
		if event.Delta != nil {
			switch event.Delta.Type {
			case "text_delta":
				out = append(out, canonical.CanonicalEvent{
					Type:  canonical.EventTextDelta,
					Index: event.Index,
					Text:  event.Delta.Text,
				})
			case "thinking_delta":
				out = append(out, canonical.CanonicalEvent{
					Type:     canonical.EventThinkingDelta,
					Index:    event.Index,
					Thinking: event.Delta.Thinking,
				})
			case "signature_delta":
				out = append(out, canonical.CanonicalEvent{
					Type:             canonical.EventThinkingDelta,
					Index:            event.Index,
					ThoughtSignature: event.Delta.Signature,
				})
			case "input_json_delta":
				out = append(out, canonical.CanonicalEvent{
					Type:         canonical.EventToolCallDelta,
					Index:        event.Index,
					ToolCallArgs: event.Delta.PartialJSON,
				})
			}
		}

	case "content_block_stop":
		out = append(out, canonical.CanonicalEvent{
			Type:  canonical.EventToolCallDone,
			Index: event.Index,
		})

	case "message_delta":
		finishReason := ""
		if event.Delta != nil && event.Delta.StopReason != "" {
			if event.Delta.StopReason == "tool_use" {
				finishReason = "tool_calls"
			} else if event.Delta.StopReason == "max_tokens" {
				finishReason = "length"
			} else {
				finishReason = "stop"
			}
		}
		var usage *canonical.Usage
		if event.Usage != nil {
			usage = usagePointer(accumulated.Canonical())
		}
		out = append(out, canonical.CanonicalEvent{
			Type:         canonical.EventMessageDelta,
			FinishReason: finishReason,
			Usage:        usage,
		})

	case "message_stop":
		out = append(out, canonical.CanonicalEvent{
			Type: canonical.EventMessageDone,
		})
	}

	return out, nil
}

func usagePointer(usage canonical.Usage) *canonical.Usage { return &usage }

// FromAnthropicRequest converts an incoming Anthropic MessageRequest into a CanonicalRequest.
func FromAnthropicRequest(req *MessageRequest) (*canonical.CanonicalRequest, error) {
	choice, err := toolnames.ParseChoice(req.ToolChoice, true)
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
			TopK:        req.TopK,
			MaxTokens:   &req.MaxTokens,
			Stop:        req.StopSequences,
		},
	}

	// System prompt
	if req.System != nil {
		switch s := req.System.(type) {
		case string:
			if s != "" {
				out.Messages = append(out.Messages, canonical.Message{
					Role:  canonical.RoleSystem,
					Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: s}},
				})
			}
		case []any:
			var combined string
			for _, item := range s {
				if itemMap, ok := item.(map[string]any); ok {
					if txt, ok := itemMap["text"].(string); ok {
						if combined != "" {
							combined += "\n\n"
						}
						combined += txt
					}
				}
			}
			if combined != "" {
				out.Messages = append(out.Messages, canonical.Message{
					Role:  canonical.RoleSystem,
					Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: combined}},
				})
			}
		}
	}

	// Tools
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, canonical.ToolDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}

	// Messages
	for _, m := range req.Messages {
		role := canonical.RoleUser
		if m.Role == "assistant" {
			role = canonical.RoleAssistant
		}

		switch c := m.Content.(type) {
		case string:
			out.Messages = append(out.Messages, canonical.Message{
				Role:  role,
				Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: c}},
			})

		case []any:
			var parts []canonical.ContentPart
			for _, blockRaw := range c {
				blockMap, ok := blockRaw.(map[string]any)
				if !ok {
					continue
				}
				bType, _ := blockMap["type"].(string)
				switch bType {
				case "text":
					if txt, ok := blockMap["text"].(string); ok {
						parts = append(parts, canonical.ContentPart{Type: canonical.PartText, Text: txt})
					}
				case "thinking":
					th, _ := blockMap["thinking"].(string)
					sig, _ := blockMap["signature"].(string)
					parts = append(parts, canonical.ContentPart{
						Type:             canonical.PartThinking,
						Thinking:         th,
						ThoughtSignature: sig,
					})
				case "redacted_thinking":
					data, _ := blockMap["data"].(string)
					parts = append(parts, canonical.ContentPart{
						Type:             canonical.PartThinking,
						ThoughtSignature: data,
					})
				case "image":
					if src, ok := blockMap["source"].(map[string]any); ok {
						mime, _ := src["media_type"].(string)
						data, _ := src["data"].(string)
						parts = append(parts, canonical.ContentPart{
							Type:           canonical.PartImage,
							ImageMediaType: mime,
							ImageData:      data,
						})
					}
				case "tool_use":
					id, _ := blockMap["id"].(string)
					name, _ := blockMap["name"].(string)
					inputJSON := "{}"
					if inputVal, ok := blockMap["input"]; ok {
						if b, err := json.Marshal(inputVal); err == nil {
							inputJSON = string(b)
						}
					}
					parts = append(parts, canonical.ContentPart{
						Type:         canonical.PartToolCall,
						ToolCallID:   id,
						ToolCallName: name,
						ToolCallArgs: inputJSON,
					})
				case "tool_result":
					toolUseID, _ := blockMap["tool_use_id"].(string)
					isErr, _ := blockMap["is_error"].(bool)
					resContent := ""
					var resultParts []canonical.ContentPart
					if resVal, ok := blockMap["content"]; ok {
						switch r := resVal.(type) {
						case string:
							resContent = r
						case []any:
							for _, item := range r {
								block, _ := item.(map[string]any)
								if block["type"] == "text" {
									text, _ := block["text"].(string)
									resultParts = append(resultParts, canonical.ContentPart{Type: canonical.PartText, Text: text})
									continue
								}
								if block["type"] == "image" {
									source, _ := block["source"].(map[string]any)
									if source["type"] == "base64" {
										data, _ := source["data"].(string)
										mime, _ := source["media_type"].(string)
										resultParts = append(resultParts, canonical.ContentPart{Type: canonical.PartImage, ImageData: data, ImageMediaType: mime})
										continue
									}
								}
								// Retain unfamiliar blocks as text instead of dropping them.
								data, _ := json.Marshal(item)
								resultParts = append(resultParts, canonical.ContentPart{Type: canonical.PartText, Text: string(data)})
							}
						default:
							b, _ := json.Marshal(r)
							resContent = string(b)
						}
					}
					// If previously accumulated parts in user turn, emit them first
					if len(parts) > 0 {
						out.Messages = append(out.Messages, canonical.Message{
							Role:  role,
							Parts: parts,
						})
						parts = nil
					}
					// Emit canonical Tool result message
					out.Messages = append(out.Messages, canonical.Message{
						Role: canonical.RoleTool,
						Parts: []canonical.ContentPart{
							{
								Type:              canonical.PartToolResult,
								ToolResultID:      toolUseID,
								ToolResultContent: resContent,
								ToolResultParts:   resultParts,
								ToolResultError:   isErr,
							},
						},
					})
				}
			}
			if len(parts) > 0 {
				out.Messages = append(out.Messages, canonical.Message{
					Role:  role,
					Parts: parts,
				})
			}
		}
	}

	return out, nil
}

// ToAnthropicResponse converts a CanonicalResponse into an Anthropic MessageResponse.
func ToAnthropicResponse(resp *canonical.CanonicalResponse) (*MessageResponse, error) {
	id := resp.ID
	if id == "" {
		id = "msg_01"
	}

	stopReason := "end_turn"
	if resp.FinishReason == "tool_calls" {
		stopReason = "tool_use"
	} else if resp.FinishReason == "length" {
		stopReason = "max_tokens"
	}

	var blocks []ContentBlock
	for _, p := range resp.Message.Parts {
		switch p.Type {
		case canonical.PartText:
			blocks = append(blocks, ContentBlock{
				Type: "text",
				Text: p.Text,
			})
		case canonical.PartThinking:
			blocks = append(blocks, ContentBlock{
				Type:     "thinking",
				Thinking: p.Thinking,
			})
		case canonical.PartToolCall:
			var inputMap map[string]any
			if p.ToolCallArgs != "" {
				_ = json.Unmarshal([]byte(p.ToolCallArgs), &inputMap)
			}
			if inputMap == nil {
				inputMap = make(map[string]any)
			}
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    p.ToolCallID,
				Name:  p.ToolCallName,
				Input: inputMap,
			})
		}
	}

	return &MessageResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      resp.Model,
		Content:    blocks,
		StopReason: stopReason,
		Usage:      FromCanonicalUsage(resp.Usage),
	}, nil
}
