package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/toolnames"
)

// Newer OpenAI models (gpt-5.x and later) reject function tools combined with
// reasoning on /v1/chat/completions ("Please use /v1/responses instead"), so
// tool-bearing requests to api.openai.com are sent to /v1/responses.

// shouldUseResponses reports whether req must go to the upstream Responses API.
func (c *Client) shouldUseResponses(req *canonical.CanonicalRequest) bool {
	if len(req.Tools) == 0 || !usesMaxCompletionTokens(strings.ToLower(req.Model)) {
		return false
	}
	model := strings.ToLower(req.Model)
	if strings.HasPrefix(model, "o") { // o-series still work with tools on chat completions
		return false
	}
	u, err := url.Parse(c.baseURL)
	return err == nil && u.Hostname() == "api.openai.com"
}

// isResponsesRequired detects the upstream hint that a request must use /v1/responses.
func isResponsesRequired(status int, body []byte) bool {
	return status == http.StatusBadRequest && bytes.Contains(body, []byte("/v1/responses"))
}

// toResponsesUpstreamRequest converts a canonical request to a Responses API body.
func toResponsesUpstreamRequest(req *canonical.CanonicalRequest) (map[string]any, *toolnames.Mapping) {
	req, names := toolnames.Normalize(req, toolnames.Constraints{MaxLength: 64, Allowed: toolnames.ASCII})
	out := map[string]any{"model": req.Model, "store": false}
	if req.Stream {
		out["stream"] = true
	}
	if sys := req.SystemPrompt(); sys != "" {
		out["instructions"] = sys
	}
	if req.Params.MaxTokens != nil {
		out["max_output_tokens"] = *req.Params.MaxTokens
	}
	if req.Thinking != nil {
		switch req.Thinking.Level {
		case "minimal", "low", "medium", "high", "xhigh":
			out["reasoning"] = map[string]any{"effort": req.Thinking.Level}
		}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": params})
		}
		out["tools"] = tools
		if choice := toolnames.WireChoice(req.ToolChoice, false); choice != nil {
			if m, ok := choice.(map[string]any); ok { // {"type":"function","function":{"name":x}}
				choice = map[string]any{"type": "function", "name": req.ToolChoice.Name}
				_ = m
			}
			out["tool_choice"] = choice
		}
	}

	var input []map[string]any
	var toolImages []map[string]any
	flush := func() {
		if len(toolImages) > 0 {
			input = append(input, map[string]any{"role": "user", "content": toolImages})
			toolImages = nil
		}
	}
	imageURL := func(p canonical.ContentPart) string {
		if p.ImageURL != "" {
			return p.ImageURL
		}
		mime := p.ImageMediaType
		if mime == "" {
			mime = "image/png"
		}
		return fmt.Sprintf("data:%s;base64,%s", mime, p.ImageData)
	}
	for _, m := range req.Messages {
		if m.Role != canonical.RoleTool {
			flush()
		}
		switch m.Role {
		case canonical.RoleUser:
			var content []map[string]any
			for _, p := range m.Parts {
				switch p.Type {
				case canonical.PartText:
					content = append(content, map[string]any{"type": "input_text", "text": p.Text})
				case canonical.PartImage:
					content = append(content, map[string]any{"type": "input_image", "image_url": imageURL(p)})
				}
			}
			if len(content) > 0 {
				input = append(input, map[string]any{"role": "user", "content": content})
			}
		case canonical.RoleAssistant:
			var text string
			var calls []map[string]any
			for _, p := range m.Parts {
				switch p.Type {
				case canonical.PartText:
					text += p.Text
				case canonical.PartToolCall:
					args := p.ToolCallArgs
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{"type": "function_call", "call_id": p.ToolCallID, "name": p.ToolCallName, "arguments": args})
				}
			}
			if text != "" {
				input = append(input, map[string]any{"role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text}}})
			}
			for _, call := range calls {
				input = append(input, call)
			}
		case canonical.RoleTool:
			for _, p := range m.Parts {
				if p.Type != canonical.PartToolResult {
					continue
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": p.ToolResultID, "output": p.ToolResultText()})
				for _, part := range p.ToolResultParts {
					if part.Type == canonical.PartImage {
						toolImages = append(toolImages,
							map[string]any{"type": "input_text", "text": "Image from tool result " + p.ToolResultID + ":"},
							map[string]any{"type": "input_image", "image_url": imageURL(part)})
					}
				}
			}
		}
	}
	flush()
	if input == nil {
		input = []map[string]any{}
	}
	out["input"] = input
	return out, names
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *responsesUsage) canonical() *canonical.Usage {
	if u == nil {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	return &canonical.Usage{
		PromptTokens:         u.InputTokens,
		CompletionTokens:     u.OutputTokens,
		TotalTokens:          total,
		CacheReadInputTokens: u.InputTokensDetails.CachedTokens,
		ReasoningTokens:      u.OutputTokensDetails.ReasoningTokens,
	}
}

type responsesOutputItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responsesObject struct {
	ID                string                `json:"id"`
	Model             string                `json:"model"`
	Status            string                `json:"status"`
	Output            []responsesOutputItem `json:"output"`
	Usage             *responsesUsage       `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (r *responsesObject) finishReason(hasToolCall bool) string {
	if r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "max_output_tokens" {
		return "length"
	}
	if hasToolCall {
		return "tool_calls"
	}
	return "stop"
}

// fromResponsesUpstream converts a non-streaming Responses object to canonical.
func fromResponsesUpstream(r *responsesObject) (*canonical.CanonicalResponse, error) {
	if r.Status == "failed" && r.Error != nil {
		return nil, fmt.Errorf("openai responses failed: %s", r.Error.Message)
	}
	msg := canonical.Message{Role: canonical.RoleAssistant}
	hasCall := false
	for _, item := range r.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					msg.Parts = append(msg.Parts, canonical.ContentPart{Type: canonical.PartText, Text: c.Text})
				}
			}
		case "function_call":
			hasCall = true
			msg.Parts = append(msg.Parts, canonical.ContentPart{Type: canonical.PartToolCall, ToolCallID: item.CallID, ToolCallName: item.Name, ToolCallArgs: item.Arguments})
		}
	}
	resp := &canonical.CanonicalResponse{ID: r.ID, Model: r.Model, Message: msg, FinishReason: r.finishReason(hasCall)}
	if u := r.Usage.canonical(); u != nil {
		resp.Usage = *u
	}
	return resp, nil
}

// readResponsesStream converts a Responses API SSE stream into canonical events.
// A response.completed/incomplete terminal event is required; bare EOF is an error.
func readResponsesStream(ctx context.Context, body io.ReadCloser) <-chan canonical.CanonicalEvent {
	out := make(chan canonical.CanonicalEvent, 64)
	go func() {
		defer close(out)
		defer body.Close()
		stop := context.AfterFunc(ctx, func() { _ = body.Close() })
		defer stop()
		send := func(ev canonical.CanonicalEvent) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- ev:
				return true
			}
		}
		fail := func(err error) { send(canonical.CanonicalEvent{Type: canonical.EventError, Error: err}) }

		var (
			msgID, model string
			nextTool     int
			toolIdx      = map[string]int{} // item id -> canonical tool index
			hasCall      bool
		)
		handle := func(payload string) (done bool) {
			var ev struct {
				Type        string               `json:"type"`
				Delta       string               `json:"delta"`
				ItemID      string               `json:"item_id"`
				Item        *responsesOutputItem `json:"item"`
				Response    *responsesObject     `json:"response"`
				Message     string               `json:"message"`
				Code        string               `json:"code"`
				OutputIndex int                  `json:"output_index"`
			}
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				fail(fmt.Errorf("invalid responses event: %w", err))
				return true
			}
			switch ev.Type {
			case "response.created":
				if ev.Response != nil {
					msgID, model = ev.Response.ID, ev.Response.Model
				}
				return !send(canonical.CanonicalEvent{Type: canonical.EventMessageStart, MessageID: msgID, Model: model})
			case "response.output_text.delta":
				return !send(canonical.CanonicalEvent{Type: canonical.EventTextDelta, MessageID: msgID, Text: ev.Delta})
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				return !send(canonical.CanonicalEvent{Type: canonical.EventThinkingDelta, MessageID: msgID, Thinking: ev.Delta})
			case "response.output_item.added":
				if ev.Item != nil && ev.Item.Type == "function_call" {
					hasCall = true
					idx := nextTool
					nextTool++
					toolIdx[ev.Item.ID] = idx
					return !send(canonical.CanonicalEvent{Type: canonical.EventToolCallStart, MessageID: msgID, Index: idx, ToolCallID: ev.Item.CallID, ToolCallName: ev.Item.Name})
				}
			case "response.function_call_arguments.delta":
				if idx, ok := toolIdx[ev.ItemID]; ok && ev.Delta != "" {
					return !send(canonical.CanonicalEvent{Type: canonical.EventToolCallDelta, MessageID: msgID, Index: idx, ToolCallArgs: ev.Delta})
				}
			case "response.completed", "response.incomplete":
				r := ev.Response
				if r == nil {
					r = &responsesObject{}
				}
				if !send(canonical.CanonicalEvent{Type: canonical.EventMessageDelta, MessageID: msgID, FinishReason: r.finishReason(hasCall)}) {
					return true
				}
				if u := r.Usage.canonical(); u != nil {
					if !send(canonical.CanonicalEvent{Type: canonical.EventMessageDelta, MessageID: msgID, Model: model, Usage: u}) {
						return true
					}
				}
				send(canonical.CanonicalEvent{Type: canonical.EventMessageDone})
				return true
			case "response.failed":
				msg := "response failed"
				if ev.Response != nil && ev.Response.Error != nil {
					msg = ev.Response.Error.Message
				}
				fail(fmt.Errorf("openai responses failed: %s", msg))
				return true
			case "error":
				fail(fmt.Errorf("upstream stream error: %s %s", ev.Code, ev.Message))
				return true
			}
			return false
		}

		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 4096), 16*1024*1024)
		var data []string
		flushData := func() bool {
			if len(data) == 0 {
				return false
			}
			payload := strings.Join(data, "\n")
			data = nil
			return handle(payload)
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if flushData() {
					return
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := scanner.Err(); err != nil {
			fail(fmt.Errorf("read responses stream: %w", err))
			return
		}
		if flushData() {
			return
		}
		fail(io.ErrUnexpectedEOF)
	}()
	return out
}

func (c *Client) newResponsesRequest(ctx context.Context, req *canonical.CanonicalRequest, stream bool) (*http.Request, *toolnames.Mapping, error) {
	body, names := toResponsesUpstreamRequest(req)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal openai responses request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if key := c.getAPIKey(req); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	return httpReq, names, nil
}

func (c *Client) executeResponses(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	httpReq, names, err := c.newResponsesRequest(ctx, req, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http execute request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai api error status %d: %s", resp.StatusCode, string(respBody))
	}
	var obj responsesObject
	if err := json.Unmarshal(respBody, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal openai responses response: %w", err)
	}
	return names.RestoreResponse(fromResponsesUpstream(&obj))
}

func (c *Client) streamResponses(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	httpReq, names, err := c.newResponsesRequest(ctx, req, true)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai stream api error %d: %s", resp.StatusCode, string(body))
	}
	return names.RestoreStream(ctx, readResponsesStream(ctx, resp.Body)), nil
}
