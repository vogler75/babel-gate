package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func TestToolResultImagesFollowAllParallelToolReplies(t *testing.T) {
	req := &canonical.CanonicalRequest{Model: "test", Messages: []canonical.Message{
		{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{
			{Type: canonical.PartToolCall, ToolCallID: "a", ToolCallName: "screenshot", ToolCallArgs: "{}"},
			{Type: canonical.PartToolCall, ToolCallID: "b", ToolCallName: "inspect", ToolCallArgs: "{}"},
		}},
		{Role: canonical.RoleTool, Parts: []canonical.ContentPart{{Type: canonical.PartToolResult, ToolResultID: "a", ToolResultParts: []canonical.ContentPart{
			{Type: canonical.PartText, Text: "Screenshot"},
			{Type: canonical.PartImage, ImageMediaType: "image/png", ImageData: "aW1hZ2U="},
		}}}},
		{Role: canonical.RoleTool, Parts: []canonical.ContentPart{{Type: canonical.PartToolResult, ToolResultID: "b", ToolResultContent: "Done"}}},
	}}
	wire, err := ToOpenAIRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 4 || wire.Messages[1].Role != "tool" || wire.Messages[2].Role != "tool" || wire.Messages[3].Role != "user" {
		t.Fatalf("images interrupted parallel tool replies: %+v", wire.Messages)
	}
	if wire.Messages[1].Content != "Screenshot" {
		t.Fatal("binary output leaked into tool text")
	}
	parts, ok := wire.Messages[3].Content.([]ContentPart)
	if !ok || len(parts) != 2 || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,aW1hZ2U=" {
		t.Fatal("tool image lost")
	}
}

func TestOpenAIBidirectional(t *testing.T) {
	req := &ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Calculate 2+2"},
		},
		Tools: []ToolDefinition{
			{
				Type: "function",
				Function: FunctionDefinition{
					Name:        "calculator",
					Description: "Calculate math expression",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"expr": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	// 1. FromOpenAIRequest -> Canonical
	canonReq, err := FromOpenAIRequest(req)
	if err != nil {
		t.Fatalf("FromOpenAIRequest error: %v", err)
	}

	if canonReq.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %s", canonReq.Model)
	}
	if canonReq.SystemPrompt() != "You are a helpful assistant." {
		t.Errorf("system prompt mismatch: %s", canonReq.SystemPrompt())
	}
	if len(canonReq.Tools) != 1 || canonReq.Tools[0].Name != "calculator" {
		t.Errorf("tools mismatch: %+v", canonReq.Tools)
	}

	// 2. Canonical -> ToOpenAIRequest
	wireReq, err := ToOpenAIRequest(canonReq)
	if err != nil {
		t.Fatalf("ToOpenAIRequest error: %v", err)
	}

	if wireReq.Model != "gpt-4o" {
		t.Errorf("wire model mismatch: %s", wireReq.Model)
	}
	if len(wireReq.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(wireReq.Messages))
	}
}

func TestOpenAIResponseTransform(t *testing.T) {
	canonResp := &canonical.CanonicalResponse{
		ID:    "resp-123",
		Model: "gpt-4o",
		Message: canonical.Message{
			Role: canonical.RoleAssistant,
			Parts: []canonical.ContentPart{
				{Type: canonical.PartText, Text: "Calling calculator"},
				{
					Type:         canonical.PartToolCall,
					ToolCallID:   "call_abc",
					ToolCallName: "calculator",
					ToolCallArgs: `{"expr":"2+2"}`,
				},
			},
		},
		FinishReason: "tool_calls",
		Usage: canonical.Usage{
			PromptTokens:     15,
			CompletionTokens: 10,
			TotalTokens:      25,
		},
	}

	oaiResp, err := ToOpenAIResponse(canonResp)
	if err != nil {
		t.Fatalf("ToOpenAIResponse error: %v", err)
	}

	if oaiResp.ID != "resp-123" {
		t.Errorf("ID mismatch: %s", oaiResp.ID)
	}
	if len(oaiResp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(oaiResp.Choices))
	}
	choice := oaiResp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish reason mismatch: %s", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(choice.Message.ToolCalls))
	}
	tc := choice.Message.ToolCalls[0]
	if tc.Function.Name != "calculator" || tc.Function.Arguments != `{"expr":"2+2"}` {
		t.Errorf("unexpected tool call: %+v", tc)
	}
}

func TestOpenAIListModelsSDCCatalog(t *testing.T) {
	sdcJSON := `{
		"service": "SDC LLM Gateway",
		"catalog": [
			{
				"tool": "CURL / Gemini",
				"base_path": "/v1beta",
				"supports_streaming": true,
				"models": ["google/gemini-2.5-flash"]
			},
			{
				"tool": "CURL / OpenAI",
				"base_path": "/v1/chat/completions",
				"supports_streaming": true,
				"models": [
					"gpt-5",
					"gpt-5-mini",
					"gpt-5.4"
				]
			},
			{
				"tool": "OpenAI SDK",
				"base_path": "Varied (/v1/chat/completions and /v1/responses)",
				"supports_streaming": true,
				"models": [
					"gpt-5",
					"gpt-5.1-codex"
				]
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

	client := NewClient("openai", "test-key", ts.URL, nil, ts.Client())
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	// Should extract OpenAI models (gpt-5, gpt-5-mini, gpt-5.4, gpt-5.1-codex), deduplicated
	expected := map[string]bool{
		"gpt-5":         true,
		"gpt-5-mini":    true,
		"gpt-5.4":       true,
		"gpt-5.1-codex": true,
	}

	if len(models) != len(expected) {
		t.Fatalf("expected %d models, got %d: %+v", len(expected), len(models), models)
	}

	for _, m := range models {
		if !expected[m.ID] {
			t.Errorf("unexpected model: %s", m.ID)
		}
	}
}
