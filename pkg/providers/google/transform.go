package google

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// ToGoogleRequest converts a CanonicalRequest into a Google GenerateContentRequest.
func ToGoogleRequest(req *canonical.CanonicalRequest) (*GenerateContentRequest, error) {
	out := &GenerateContentRequest{}

	// System Instruction
	if sys := req.SystemPrompt(); sys != "" {
		out.SystemInstruction = &Content{
			Role: "user",
			Parts: []Part{
				{Text: sys},
			},
		}
	}

	// Tools
	if len(req.Tools) > 0 {
		var decls []FunctionDeclaration
		for _, t := range req.Tools {
			var params map[string]any
			if t.Parameters != nil {
				if sanitized, ok := sanitizeGoogleSchema(t.Parameters).(map[string]any); ok {
					params = sanitized
				}
			}
			decls = append(decls, FunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			})
		}
		out.Tools = []Tool{
			{FunctionDeclarations: decls},
		}
	}

	// GenerationConfig
	if req.Params.Temperature != nil || req.Params.TopP != nil || req.Params.TopK != nil || req.Params.MaxTokens != nil || len(req.Params.Stop) > 0 {
		out.GenerationConfig = &GenerationConfig{
			Temperature:     req.Params.Temperature,
			TopP:            req.Params.TopP,
			TopK:            req.Params.TopK,
			MaxOutputTokens: req.Params.MaxTokens,
			StopSequences:   req.Params.Stop,
		}
	}

	// Build map of toolCallID -> toolCallName from previous assistant turns
	toolIDToName := make(map[string]string)
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Type == canonical.PartToolCall && p.ToolCallID != "" && p.ToolCallName != "" {
				toolIDToName[p.ToolCallID] = p.ToolCallName
			}
		}
	}

	// Messages
	for _, m := range req.NonSystemMessages() {
		switch m.Role {
		case canonical.RoleUser:
			var parts []Part
			for _, p := range m.Parts {
				switch p.Type {
				case canonical.PartText:
					if p.Text != "" {
						parts = append(parts, Part{Text: p.Text})
					}
				case canonical.PartImage:
					parts = append(parts, Part{
						InlineData: &Blob{
							MimeType: p.ImageMediaType,
							Data:     p.ImageData,
						},
					})
				case canonical.PartToolResult:
					fnName := toolIDToName[p.ToolResultID]
					if fnName == "" {
						fnName = p.ToolResultID
					}
					parts = append(parts, googleToolResultParts(p, fnName, req.Model)...)
				}
			}
			if len(parts) > 0 {
				out.Contents = append(out.Contents, Content{
					Role:  "user",
					Parts: parts,
				})
			}

		case canonical.RoleAssistant:
			var parts []Part
			for _, p := range m.Parts {
				switch p.Type {
				case canonical.PartText:
					if p.Text != "" {
						parts = append(parts, Part{Text: p.Text})
					}
				case canonical.PartThinking:
					if p.Thinking != "" {
						parts = append(parts, Part{Text: p.Thinking, Thought: true})
					}
				case canonical.PartToolCall:
					var args map[string]any
					if p.ToolCallArgs != "" {
						_ = json.Unmarshal([]byte(p.ToolCallArgs), &args)
					}
					if args == nil {
						args = make(map[string]any)
					}

					// Resolve thought signature:
					// 1. From canonical part if present
					// 2. From cache by ToolCallID and tool name
					// 3. Fallback to Google's official sentinel "skip_thought_signature_validator"
					sig := p.ThoughtSignature
					if sig == "" && p.ToolCallID != "" {
						sig = GetThoughtSignature(p.ToolCallID)
					}
					if sig == "" {
						sig = "skip_thought_signature_validator"
					}

					parts = append(parts, Part{
						FunctionCall: &FunctionCall{
							ID:   p.ToolCallID,
							Name: p.ToolCallName,
							Args: args,
						},
						ThoughtSignature: sig,
					})
				}
			}
			if len(parts) > 0 {
				out.Contents = append(out.Contents, Content{
					Role:  "model",
					Parts: parts,
				})
			}

		case canonical.RoleTool:
			var parts []Part
			for _, p := range m.Parts {
				if p.Type == canonical.PartToolResult {
					fnName := toolIDToName[p.ToolResultID]
					if fnName == "" {
						fnName = p.ToolResultID
					}
					parts = append(parts, googleToolResultParts(p, fnName, req.Model)...)
				}
			}
			if len(parts) > 0 {
				out.Contents = append(out.Contents, Content{
					Role:  "user",
					Parts: parts,
				})
			}
		}
	}

	out.Contents = mergeConsecutiveContents(out.Contents)
	// Claude Code can append text reminders after tool results. Gemini rejects
	// these mixed turns as ending with a model turn unless function responses
	// come last. Normalize after merging so separate user/tool messages are
	// handled too, preserving order within both groups and all original parts.
	for i := range out.Contents {
		content := &out.Contents[i]
		if content.Role != "user" {
			continue
		}
		var responses []Part
		var other []Part
		for _, part := range content.Parts {
			if part.FunctionResponse != nil {
				responses = append(responses, part)
			} else {
				other = append(other, part)
			}
		}
		if len(responses) > 0 && len(other) > 0 {
			content.Parts = append(other, responses...)
		}
	}

	// Google Gemini requires contents to start with a user turn.
	if len(out.Contents) > 0 && out.Contents[0].Role == "model" {
		out.Contents = append([]Content{
			{
				Role:  "user",
				Parts: []Part{{Text: "Hello"}},
			},
		}, out.Contents...)
	}

	// Google Gemini strictly forbids requests ending with a model turn.
	// If the request ends with a model turn (e.g. from an assistant prefill or truncated turn),
	// normalize it so that the conversation always ends with a user turn.
	for len(out.Contents) > 0 && out.Contents[len(out.Contents)-1].Role == "model" {
		lastIdx := len(out.Contents) - 1
		last := out.Contents[lastIdx]

		hasToolCalls := false
		hasNonEmptyContent := false
		for _, p := range last.Parts {
			if p.FunctionCall != nil {
				hasToolCalls = true
			}
			if p.Text != "" || p.InlineData != nil {
				hasNonEmptyContent = true
			}
		}

		if !hasToolCalls && !hasNonEmptyContent {
			// Entirely empty prefill turn, drop it
			out.Contents = out.Contents[:lastIdx]
			continue
		}

		if hasToolCalls {
			// A model turn with tool calls MUST have matching FunctionResponse(s) in the following user turn.
			// If tool calls are unanswered at the end of the history, we cannot just append Text: "Continue"
			// because Gemini strictly rejects text responses to function calls with:
			// "Requests ending with a model turn are not supported."
			var respParts []Part
			for _, p := range last.Parts {
				if p.FunctionCall != nil {
					respParts = append(respParts, Part{
						FunctionResponse: &FunctionResponse{
							ID:       p.FunctionCall.ID,
							Name:     p.FunctionCall.Name,
							Response: map[string]any{"output": "cancelled or unavailable"},
						},
					})
				}
			}
			out.Contents = append(out.Contents, Content{
				Role:  "user",
				Parts: respParts,
			})
			break
		}

		// Non-empty prefill or trailing model turn without tool calls: append user turn to prompt completion
		out.Contents = append(out.Contents, Content{
			Role:  "user",
			Parts: []Part{{Text: "Continue"}},
		})
		break
	}

	if len(out.Contents) == 0 {
		out.Contents = []Content{
			{
				Role:  "user",
				Parts: []Part{{Text: "Hello"}},
			},
		}
	}

	return out, nil
}

func googleToolResultParts(p canonical.ContentPart, name, model string) []Part {
	response := &FunctionResponse{ID: p.ToolResultID, Name: name, Response: map[string]any{"output": p.ToolResultText()}}
	if p.ToolResultError {
		response.Response["error"] = true
	}
	var images []Part
	for _, part := range p.ToolResultParts {
		if part.Type == canonical.PartImage {
			mime := part.ImageMediaType
			if mime == "" {
				mime = "image/png"
			}
			images = append(images, Part{InlineData: &Blob{MimeType: mime, Data: part.ImageData}})
		}
	}
	model = strings.TrimPrefix(strings.TrimPrefix(model, "google/"), "models/")
	if strings.HasPrefix(model, "gemini-3") {
		// Gemini 3 supports images inside their corresponding tool response.
		response.Parts = images
		return []Part{{FunctionResponse: response}}
	}
	// Earlier models accept images as ordinary user parts beside the tool
	// response. Keep the response last, as required by Gemini turn ordering.
	return append(images, Part{FunctionResponse: response})
}

func sanitizeGoogleSchema(v any) any {
	switch val := v.(type) {
	case map[string]any:
		res := make(map[string]any)

		// Check anyOf / oneOf / allOf and fold first option
		for _, unionKey := range []string{"anyOf", "oneOf", "allOf"} {
			if unionArr, ok := val[unionKey].([]any); ok && len(unionArr) > 0 {
				if first, ok := unionArr[0].(map[string]any); ok {
					for fk, fv := range first {
						if _, exists := val[fk]; !exists {
							val[fk] = fv
						}
					}
					res["nullable"] = true
				}
			}
		}

		// Handle type
		if tVal, exists := val["type"]; exists {
			if typeStr, ok := tVal.(string); ok {
				res["type"] = strings.ToUpper(typeStr)
			} else if typeArr, ok := tVal.([]any); ok {
				for _, item := range typeArr {
					if s, ok := item.(string); ok && s != "null" {
						res["type"] = strings.ToUpper(s)
						res["nullable"] = true
						break
					}
				}
			}
		}
		if _, exists := res["type"]; !exists {
			if _, hasProps := val["properties"]; hasProps {
				res["type"] = "OBJECT"
			} else {
				res["type"] = "STRING"
			}
		}

		if desc, ok := val["description"].(string); ok && desc != "" {
			res["description"] = desc
		}

		if nullable, ok := val["nullable"].(bool); ok {
			res["nullable"] = nullable
		}

		if req, ok := val["required"].([]any); ok {
			var reqStrings []string
			for _, r := range req {
				if rs, ok := r.(string); ok {
					reqStrings = append(reqStrings, rs)
				}
			}
			if len(reqStrings) > 0 {
				res["required"] = reqStrings
			}
		} else if req, ok := val["required"].([]string); ok && len(req) > 0 {
			res["required"] = req
		}

		if props, ok := val["properties"].(map[string]any); ok {
			cleanProps := make(map[string]any)
			for pk, pv := range props {
				cleanProps[pk] = sanitizeGoogleSchema(pv)
			}
			res["properties"] = cleanProps
		}

		if items, exists := val["items"]; exists {
			res["items"] = sanitizeGoogleSchema(items)
		}

		if enumVal, ok := val["enum"].([]any); ok {
			var enumStrs []string
			for _, e := range enumVal {
				enumStrs = append(enumStrs, fmt.Sprintf("%v", e))
			}
			if len(enumStrs) > 0 {
				res["enum"] = enumStrs
			}
		}

		return res

	case []any:
		res := make([]any, len(val))
		for i, child := range val {
			res[i] = sanitizeGoogleSchema(child)
		}
		return res

	default:
		return val
	}
}

func mergeConsecutiveContents(contents []Content) []Content {
	if len(contents) <= 1 {
		return contents
	}
	var merged []Content
	for _, c := range contents {
		if len(merged) > 0 && merged[len(merged)-1].Role == c.Role {
			merged[len(merged)-1].Parts = append(merged[len(merged)-1].Parts, c.Parts...)
		} else {
			merged = append(merged, c)
		}
	}
	return merged
}

func sanitizeToolID(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// FromGoogleResponse converts a Google GenerateContentResponse into a CanonicalResponse.
func FromGoogleResponse(resp *GenerateContentResponse, model string) (*canonical.CanonicalResponse, error) {
	if len(resp.Candidates) == 0 {
		return nil, fmt.Errorf("no candidates returned by Google Gemini")
	}

	cand := resp.Candidates[0]
	msg := canonical.Message{
		Role: canonical.RoleAssistant,
	}

	finishReason := "stop"
	hasToolCalls := false
	latestSignature := ""

	for i, part := range cand.Content.Parts {
		if part.ThoughtSignature != "" {
			latestSignature = part.ThoughtSignature
		}

		if part.Text != "" {
			if part.Thought {
				msg.Parts = append(msg.Parts, canonical.ContentPart{
					Type:     canonical.PartThinking,
					Thinking: part.Text,
				})
			} else {
				msg.Parts = append(msg.Parts, canonical.ContentPart{
					Type: canonical.PartText,
					Text: part.Text,
				})
			}
		}
		if part.FunctionCall != nil {
			hasToolCalls = true
			argsJSON, _ := json.Marshal(part.FunctionCall.Args)

			callID := part.FunctionCall.ID
			if callID == "" {
				callID = fmt.Sprintf("call_%s_%d", sanitizeToolID(part.FunctionCall.Name), i)
			}

			sig := part.ThoughtSignature
			if sig == "" {
				sig = latestSignature
			}
			if sig != "" {
				StoreThoughtSignature(callID, sig)
			}

			msg.Parts = append(msg.Parts, canonical.ContentPart{
				Type:             canonical.PartToolCall,
				ToolCallID:       callID,
				ToolCallName:     part.FunctionCall.Name,
				ToolCallArgs:     string(argsJSON),
				ThoughtSignature: sig,
			})
		}
	}

	if hasToolCalls {
		finishReason = "tool_calls"
	} else if strings.EqualFold(cand.FinishReason, "MAX_TOKENS") {
		finishReason = "length"
	}

	canonicalResp := &canonical.CanonicalResponse{
		ID:           fmt.Sprintf("gemini-%d", len(msg.Parts)),
		Model:        model,
		Message:      msg,
		FinishReason: finishReason,
	}

	if resp.UsageMetadata != nil {
		canonicalResp.Usage = canonical.Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		}
	}

	return canonicalResp, nil
}

// ParseGoogleStreamEvent converts a Google Gemini stream chunk into canonical events.
func ParseGoogleStreamEvent(data []byte, model string) ([]canonical.CanonicalEvent, error) {
	var resp GenerateContentResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	var events []canonical.CanonicalEvent

	if resp.UsageMetadata != nil {
		events = append(events, canonical.CanonicalEvent{
			Type:  canonical.EventMessageDelta,
			Model: model,
			Usage: &canonical.Usage{
				PromptTokens:     resp.UsageMetadata.PromptTokenCount,
				CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      resp.UsageMetadata.TotalTokenCount,
			},
		})
	}

	for _, cand := range resp.Candidates {
		firstEvent := len(events)
		latestSignature := ""
		for i, part := range cand.Content.Parts {
			if part.ThoughtSignature != "" {
				latestSignature = part.ThoughtSignature
			}

			if part.Text != "" {
				if part.Thought {
					events = append(events, canonical.CanonicalEvent{
						Type:     canonical.EventThinkingDelta,
						Index:    cand.Index,
						Thinking: part.Text,
						Model:    model,
					})
				} else {
					events = append(events, canonical.CanonicalEvent{
						Type:  canonical.EventTextDelta,
						Index: cand.Index,
						Text:  part.Text,
						Model: model,
					})
				}
			}
			if part.FunctionCall != nil {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)

				callID := part.FunctionCall.ID
				if callID == "" {
					callID = "call_" + rand.Text()
				}

				sig := part.ThoughtSignature
				if sig == "" {
					sig = latestSignature
				}
				if sig != "" {
					StoreThoughtSignature(callID, sig)
				}

				events = append(events, canonical.CanonicalEvent{
					Type:             canonical.EventToolCallStart,
					Index:            i,
					ToolCallID:       callID,
					ToolCallName:     part.FunctionCall.Name,
					ThoughtSignature: sig,
					Model:            model,
				})
				events = append(events, canonical.CanonicalEvent{
					Type:             canonical.EventToolCallDelta,
					Index:            i,
					ToolCallID:       callID,
					ToolCallArgs:     string(argsJSON),
					ThoughtSignature: sig,
					Model:            model,
				})
				events = append(events, canonical.CanonicalEvent{
					Type:             canonical.EventToolCallDone,
					Index:            i,
					ToolCallID:       callID,
					ThoughtSignature: sig,
					Model:            model,
				})
			}
		}

		if cand.FinishReason != "" {
			reason := "stop"
			if cand.FinishReason == "MAX_TOKENS" {
				reason = "length"
			}
			events = append(events, canonical.CanonicalEvent{
				Type:         canonical.EventMessageDelta,
				Index:        cand.Index,
				FinishReason: reason,
				Model:        model,
			})
		}
		for i := firstEvent; i < len(events); i++ {
			events[i].CandidateIndex = cand.Index
		}
	}

	return events, nil
}

// FromGoogleRequest converts an incoming Google GenerateContentRequest into a CanonicalRequest.
func FromGoogleRequest(req *GenerateContentRequest, model string) (*canonical.CanonicalRequest, error) {
	out := &canonical.CanonicalRequest{
		Model: model,
	}

	if req.GenerationConfig != nil {
		out.Params = canonical.Parameters{
			Temperature: req.GenerationConfig.Temperature,
			TopP:        req.GenerationConfig.TopP,
			TopK:        req.GenerationConfig.TopK,
			MaxTokens:   req.GenerationConfig.MaxOutputTokens,
			Stop:        req.GenerationConfig.StopSequences,
		}
	}

	if req.SystemInstruction != nil {
		var sys string
		for _, p := range req.SystemInstruction.Parts {
			sys += p.Text
		}
		if sys != "" {
			out.Messages = append(out.Messages, canonical.Message{
				Role:  canonical.RoleSystem,
				Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: sys}},
			})
		}
	}

	for _, t := range req.Tools {
		for _, decl := range t.FunctionDeclarations {
			out.Tools = append(out.Tools, canonical.ToolDeclaration{
				Name:        decl.Name,
				Description: decl.Description,
				Parameters:  decl.Parameters,
			})
		}
	}

	pending := make(map[string]string) // unresolved call ID -> name
	usedIDs := make(map[string]bool)
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil && p.FunctionCall.ID != "" {
				usedIDs[p.FunctionCall.ID] = true
			}
		}
	}
	for _, c := range req.Contents {
		role := canonical.RoleUser
		if c.Role == "model" {
			role = canonical.RoleAssistant
		}

		var parts []canonical.ContentPart
		flushParts := func() {
			if len(parts) > 0 {
				out.Messages = append(out.Messages, canonical.Message{Role: role, Parts: parts})
				parts = nil
			}
		}
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				flushParts()
				resultID := p.FunctionResponse.ID
				if resultID == "" {
					for id, name := range pending {
						if name == p.FunctionResponse.Name {
							if resultID != "" {
								return nil, fmt.Errorf("ambiguous function response %q: supply an ID", name)
							}
							resultID = id
						}
					}
					if resultID == "" {
						return nil, fmt.Errorf("function response %q has no preceding matching call", p.FunctionResponse.Name)
					}
				}
				delete(pending, resultID)
				respStr := ""
				if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
					respStr = string(b)
				}
				var resultParts []canonical.ContentPart
				if len(p.FunctionResponse.Parts) > 0 {
					resultParts = append(resultParts, canonical.ContentPart{Type: canonical.PartText, Text: respStr})
					for _, part := range p.FunctionResponse.Parts {
						if part.InlineData != nil {
							resultParts = append(resultParts, canonical.ContentPart{Type: canonical.PartImage, ImageMediaType: part.InlineData.MimeType, ImageData: part.InlineData.Data})
						}
					}
				}
				out.Messages = append(out.Messages, canonical.Message{
					Role: canonical.RoleTool,
					Parts: []canonical.ContentPart{
						{
							Type:              canonical.PartToolResult,
							ToolResultID:      resultID,
							ToolResultContent: respStr,
							ToolResultParts:   resultParts,
						},
					},
				})
				continue
			}

			if p.Text != "" {
				parts = append(parts, canonical.ContentPart{
					Type: canonical.PartText,
					Text: p.Text,
				})
			}
			if p.InlineData != nil {
				parts = append(parts, canonical.ContentPart{
					Type:           canonical.PartImage,
					ImageMediaType: p.InlineData.MimeType,
					ImageData:      p.InlineData.Data,
				})
			}
			if p.FunctionCall != nil {
				argsJSON, _ := json.Marshal(p.FunctionCall.Args)
				callID := p.FunctionCall.ID
				if callID == "" {
					for n := len(usedIDs); ; n++ {
						callID = fmt.Sprintf("call_%s_%d", sanitizeToolID(p.FunctionCall.Name), n)
						if !usedIDs[callID] {
							break
						}
					}
				}
				usedIDs[callID] = true
				pending[callID] = p.FunctionCall.Name
				parts = append(parts, canonical.ContentPart{
					Type:             canonical.PartToolCall,
					ToolCallID:       callID,
					ToolCallName:     p.FunctionCall.Name,
					ToolCallArgs:     string(argsJSON),
					ThoughtSignature: p.ThoughtSignature,
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

	return out, nil
}

// ToGoogleResponse converts a CanonicalResponse into a Google GenerateContentResponse.
func ToGoogleResponse(resp *canonical.CanonicalResponse) (*GenerateContentResponse, error) {
	var parts []Part
	for _, p := range resp.Message.Parts {
		switch p.Type {
		case canonical.PartText:
			parts = append(parts, Part{Text: p.Text})
		case canonical.PartThinking:
			if p.Thinking != "" {
				parts = append(parts, Part{Text: p.Thinking, Thought: true})
			}
		case canonical.PartToolCall:
			var args map[string]any
			if p.ToolCallArgs != "" {
				_ = json.Unmarshal([]byte(p.ToolCallArgs), &args)
			}
			if args == nil {
				args = make(map[string]any)
			}
			sig := p.ThoughtSignature
			if sig == "" && p.ToolCallID != "" {
				sig = GetThoughtSignature(p.ToolCallID)
			}
			if sig == "" {
				sig = "skip_thought_signature_validator"
			}
			parts = append(parts, Part{
				FunctionCall: &FunctionCall{
					ID:   p.ToolCallID,
					Name: p.ToolCallName,
					Args: args,
				},
				ThoughtSignature: sig,
			})
		}
	}

	finishReason := "STOP"
	if resp.FinishReason == "length" {
		finishReason = "MAX_TOKENS"
	}

	return &GenerateContentResponse{
		Candidates: []Candidate{
			{
				Content: Content{
					Role:  "model",
					Parts: parts,
				},
				FinishReason: finishReason,
				Index:        0,
			},
		},
		UsageMetadata: &UsageMetadata{
			PromptTokenCount:     resp.Usage.PromptTokens,
			CandidatesTokenCount: resp.Usage.CompletionTokens,
			TotalTokenCount:      resp.Usage.TotalTokens,
		},
	}, nil
}
