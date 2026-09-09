package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func TestCachedUsageRoundTripAndStreamingCorrections(t *testing.T) {
	want := Usage{InputTokens: 363, CacheReadInputTokens: 100000, CacheCreationInputTokens: 7137, OutputTokens: 5}
	response, err := FromAnthropicResponse(&MessageResponse{Usage: want})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.PromptTokens != 107500 || response.Usage.TotalTokens != 107505 {
		t.Fatalf("cached prompt undercounted: %+v", response.Usage)
	}
	roundTrip, err := ToAnthropicResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Usage != want {
		t.Fatalf("cache double-counted/lost: %+v", roundTrip.Usage)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":100000,"cache_creation_input_tokens":7137}}}`,
			`data: {"type":"message_delta","usage":{"input_tokens":363,"output_tokens":5}}`,
			`data: {"type":"message_delta","usage":{"input_tokens":0,"output_tokens":6}}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n") + "\n\n"))
	}))
	defer srv.Close()
	stream, err := NewClient("mock", "", srv.URL, nil, srv.Client()).Stream(context.Background(), &canonical.CanonicalRequest{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var counts []int
	for event := range stream {
		if event.Usage != nil {
			counts = append(counts, event.Usage.PromptTokens)
		}
	}
	if len(counts) != 3 || counts[0] != 107237 || counts[1] != 107500 || counts[2] != 107137 {
		t.Fatalf("cumulative usage lost omitted fields/explicit zero: %v", counts)
	}
}

func TestAnthropicBidirectional(t *testing.T) {
	req := &MessageRequest{
		Model:  "claude-3-7-sonnet-20250219",
		System: "You are Claude Code.",
		Messages: []Message{
			{
				Role:    "user",
				Content: "Run test",
			},
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "text", "text": "Running test..."},
					map[string]any{"type": "tool_use", "id": "toolu_01", "name": "bash", "input": map[string]any{"command": "go test ./..."}},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_01", "content": "PASS"},
				},
			},
		},
		Tools: []ToolDefinition{
			{
				Name:        "bash",
				Description: "Run shell command",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	// 1. FromAnthropicRequest -> Canonical
	canonReq, err := FromAnthropicRequest(req)
	if err != nil {
		t.Fatalf("FromAnthropicRequest failed: %v", err)
	}

	if canonReq.Model != "claude-3-7-sonnet-20250219" {
		t.Errorf("model mismatch: %s", canonReq.Model)
	}
	if canonReq.SystemPrompt() != "You are Claude Code." {
		t.Errorf("system prompt mismatch: %s", canonReq.SystemPrompt())
	}
	if len(canonReq.Tools) != 1 || canonReq.Tools[0].Name != "bash" {
		t.Errorf("tools mismatch: %+v", canonReq.Tools)
	}

	// Verify messages: system, user, assistant (with tool_use), tool (with tool_result)
	if len(canonReq.Messages) != 4 {
		t.Fatalf("expected 4 canonical messages, got %d", len(canonReq.Messages))
	}

	// 2. Canonical -> ToAnthropicRequest
	wireReq, err := ToAnthropicRequest(canonReq)
	if err != nil {
		t.Fatalf("ToAnthropicRequest failed: %v", err)
	}

	if wireReq.System != "You are Claude Code." {
		t.Errorf("wire system mismatch: %v", wireReq.System)
	}
	if len(wireReq.Messages) != 3 {
		t.Fatalf("expected 3 wire messages, got %d", len(wireReq.Messages))
	}
}

func TestAnthropicResponseTransform(t *testing.T) {
	canonResp := &canonical.CanonicalResponse{
		ID:    "msg_test123",
		Model: "claude-3-7-sonnet-20250219",
		Message: canonical.Message{
			Role: canonical.RoleAssistant,
			Parts: []canonical.ContentPart{
				{Type: canonical.PartThinking, Thinking: "Analyzing the problem"},
				{Type: canonical.PartText, Text: "Here is the result"},
				{
					Type:         canonical.PartToolCall,
					ToolCallID:   "call_999",
					ToolCallName: "readFile",
					ToolCallArgs: `{"path":"main.go"}`,
				},
			},
		},
		FinishReason: "tool_calls",
		Usage: canonical.Usage{
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
		},
	}

	anthResp, err := ToAnthropicResponse(canonResp)
	if err != nil {
		t.Fatalf("ToAnthropicResponse failed: %v", err)
	}

	if anthResp.ID != "msg_test123" {
		t.Errorf("ID mismatch: %s", anthResp.ID)
	}
	if anthResp.StopReason != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %s", anthResp.StopReason)
	}
	if len(anthResp.Content) != 3 {
		t.Fatalf("expected 3 content blocks, got %d", len(anthResp.Content))
	}
	if anthResp.Content[0].Type != "thinking" || anthResp.Content[0].Thinking != "Analyzing the problem" {
		t.Errorf("thinking block mismatch: %+v", anthResp.Content[0])
	}
	if anthResp.Content[2].Type != "tool_use" || anthResp.Content[2].Name != "readFile" {
		t.Errorf("tool_use block mismatch: %+v", anthResp.Content[2])
	}
}

func TestAnthropicListModelsSDCCatalog(t *testing.T) {
	sdcJSON := `{
		"service": "SDC LLM Gateway",
		"catalog": [
			{
				"tool": "CURL / Anthropic",
				"base_path": "/v1/messages",
				"supports_streaming": true,
				"models": [
					"claude-opus-4-5@20251101",
					"claude-sonnet-4-5@20250929"
				]
			},
			{
				"tool": "CURL / OpenAI",
				"base_path": "/v1/chat/completions",
				"supports_streaming": true,
				"models": ["gpt-5"]
			}
		]
	}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(sdcJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client := NewClient("anthropic", "test-key", ts.URL+"/v1", nil, ts.Client())
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(models), models)
	}

	expected := []string{"claude-opus-4-5@20251101", "claude-sonnet-4-5@20250929"}
	for i, exp := range expected {
		if models[i].ID != exp {
			t.Errorf("expected model %d ID to be %q, got %q", i, exp, models[i].ID)
		}
	}
}

func TestThinkingBlockMarshalJSON(t *testing.T) {
	// 1. Thinking block with empty string must NOT omit thinking field
	blockEmpty := ContentBlock{
		Type:     "thinking",
		Thinking: "",
	}
	dataEmpty, err := json.Marshal(blockEmpty)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	expectedEmpty := `{"thinking":"","type":"thinking"}`
	if string(dataEmpty) != expectedEmpty {
		t.Errorf("expected %s, got %s", expectedEmpty, string(dataEmpty))
	}

	// 2. Thinking block with content and signature
	blockWithSig := ContentBlock{
		Type:      "thinking",
		Thinking:  "pondering...",
		Signature: "sig_abc123",
	}
	dataWithSig, err := json.Marshal(blockWithSig)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var resMap map[string]string
	if err := json.Unmarshal(dataWithSig, &resMap); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if resMap["type"] != "thinking" || resMap["thinking"] != "pondering..." || resMap["signature"] != "sig_abc123" {
		t.Errorf("unexpected json: %s", string(dataWithSig))
	}

	// 3. Text block must NOT contain thinking field
	textBlock := ContentBlock{
		Type: "text",
		Text: "hello",
	}
	textData, err := json.Marshal(textBlock)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var textMap map[string]any
	if err := json.Unmarshal(textData, &textMap); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := textMap["thinking"]; ok {
		t.Errorf("thinking field should not be present in text block: %s", string(textData))
	}
}

func TestThinkingSignaturePreservation(t *testing.T) {
	rawJSON := `{
		"model": "claude-3-7-sonnet-20250219",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": "thought details",
						"signature": "sig_cryptographic_test"
					}
				]
			}
		],
		"max_tokens": 1000
	}`

	var req MessageRequest
	if err := json.Unmarshal([]byte(rawJSON), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	canonReq, err := FromAnthropicRequest(&req)
	if err != nil {
		t.Fatalf("FromAnthropicRequest failed: %v", err)
	}

	if len(canonReq.Messages) != 1 || len(canonReq.Messages[0].Parts) != 1 {
		t.Fatalf("expected 1 msg and 1 part, got: %+v", canonReq.Messages)
	}

	part := canonReq.Messages[0].Parts[0]
	if part.Type != canonical.PartThinking || part.Thinking != "thought details" || part.ThoughtSignature != "sig_cryptographic_test" {
		t.Errorf("canonical part mismatch: %+v", part)
	}

	toAnthReq, err := ToAnthropicRequest(canonReq)
	if err != nil {
		t.Fatalf("ToAnthropicRequest failed: %v", err)
	}

	anthBlocks, ok := toAnthReq.Messages[0].Content.([]ContentBlock)
	if !ok || len(anthBlocks) != 1 {
		t.Fatalf("expected []ContentBlock with 1 item, got: %+v", toAnthReq.Messages[0].Content)
	}
	if anthBlocks[0].Signature != "sig_cryptographic_test" {
		t.Errorf("signature not preserved in ToAnthropicRequest: %+v", anthBlocks[0])
	}
}
