package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

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

