package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func TestGoogleBidirectional(t *testing.T) {
	req := &GenerateContentRequest{
		Contents: []Content{
			{
				Role: "user",
				Parts: []Part{
					{Text: "What is the weather?"},
				},
			},
			{
				Role: "model",
				Parts: []Part{
					{
						FunctionCall: &FunctionCall{
							Name: "getWeather",
							Args: map[string]any{"location": "San Francisco"},
						},
					},
				},
			},
			{
				Role: "user",
				Parts: []Part{
					{
						FunctionResponse: &FunctionResponse{
							Name:     "getWeather",
							Response: map[string]any{"temp": "18C"},
						},
					},
				},
			},
		},
		SystemInstruction: &Content{
			Parts: []Part{{Text: "Act as weather assistant"}},
		},
		Tools: []Tool{
			{
				FunctionDeclarations: []FunctionDeclaration{
					{
						Name:        "getWeather",
						Description: "Get weather for location",
						Parameters: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"location": map[string]any{"type": "string"},
							},
						},
					},
				},
			},
		},
	}

	// 1. FromGoogleRequest -> Canonical
	canonReq, err := FromGoogleRequest(req, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("FromGoogleRequest failed: %v", err)
	}

	if canonReq.Model != "gemini-2.5-pro" {
		t.Errorf("model mismatch: %s", canonReq.Model)
	}
	if canonReq.SystemPrompt() != "Act as weather assistant" {
		t.Errorf("system prompt mismatch: %s", canonReq.SystemPrompt())
	}
	if len(canonReq.Tools) != 1 || canonReq.Tools[0].Name != "getWeather" {
		t.Errorf("tools mismatch: %+v", canonReq.Tools)
	}

	// 2. Canonical -> ToGoogleRequest
	wireReq, err := ToGoogleRequest(canonReq)
	if err != nil {
		t.Fatalf("ToGoogleRequest failed: %v", err)
	}

	if len(wireReq.Contents) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(wireReq.Contents))
	}
	if wireReq.SystemInstruction == nil || wireReq.SystemInstruction.Parts[0].Text != "Act as weather assistant" {
		t.Errorf("systemInstruction mismatch: %+v", wireReq.SystemInstruction)
	}
}

func TestGoogleResponseTransform(t *testing.T) {
	canonResp := &canonical.CanonicalResponse{
		ID:    "gemini-1",
		Model: "gemini-2.5-pro",
		Message: canonical.Message{
			Role: canonical.RoleAssistant,
			Parts: []canonical.ContentPart{
				{Type: canonical.PartText, Text: "Checking weather"},
				{
					Type:         canonical.PartToolCall,
					ToolCallID:   "call_1",
					ToolCallName: "getWeather",
					ToolCallArgs: `{"city":"Berlin"}`,
				},
			},
		},
		FinishReason: "tool_calls",
		Usage: canonical.Usage{
			PromptTokens:     20,
			CompletionTokens: 30,
			TotalTokens:      50,
		},
	}

	googleResp, err := ToGoogleResponse(canonResp)
	if err != nil {
		t.Fatalf("ToGoogleResponse failed: %v", err)
	}

	if len(googleResp.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(googleResp.Candidates))
	}
	cand := googleResp.Candidates[0]
	if len(cand.Content.Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(cand.Content.Parts))
	}
	if cand.Content.Parts[0].Text != "Checking weather" {
		t.Errorf("part 0 text mismatch: %s", cand.Content.Parts[0].Text)
	}
	if cand.Content.Parts[1].FunctionCall == nil || cand.Content.Parts[1].FunctionCall.Name != "getWeather" {
		t.Errorf("part 1 functionCall mismatch: %+v", cand.Content.Parts[1])
	}
}

func TestGoogleListModelsSDCCatalog(t *testing.T) {
	sdcJSON := `{
		"service": "SDC LLM Gateway",
		"catalog": [
			{
				"tool": "CURL / Gemini",
				"base_path": "/v1beta",
				"supports_streaming": true,
				"models": [
					"google/gemini-2.5-flash",
					"google/gemini-2.5-pro",
					"google/gemini-3.7-flash"
				]
			},
			{
				"tool": "CURL / OpenAI",
				"base_path": "/v1/chat/completions",
				"supports_streaming": true,
				"models": ["gpt-5", "gpt-5-mini"]
			}
		]
	}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/models" {
			// Simulate gateway returning 401 on /v1beta/models (e.g. Apigee key expired), only serving catalog on /v1/models
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"fault":{"faultstring":"Key Expired"}}`))
			return
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(sdcJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client := NewClient("google", "test-key", ts.URL, nil, ts.Client())
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d: %+v", len(models), models)
	}

	expected := []string{"gemini-2.5-flash", "gemini-2.5-pro", "gemini-3.7-flash"}
	for i, exp := range expected {
		if models[i].ID != exp {
			t.Errorf("expected model %d ID to be %q, got %q", i, exp, models[i].ID)
		}
	}
}

func TestGoogleThoughtSignature_CacheAndRestore(t *testing.T) {
	fakeSig := "EpoGCpcGAXLI2nx9...encrypted_signature_bytes..."

	// 1. Google returns a response with a thought_signature on a functionCall
	googleResp := &GenerateContentResponse{
		Candidates: []Candidate{
			{
				Content: Content{
					Role: "model",
					Parts: []Part{
						{
							FunctionCall: &FunctionCall{
								Name: "default_api:Bash",
								Args: map[string]any{"command": "git status"},
							},
							ThoughtSignature: fakeSig,
						},
					},
				},
				FinishReason: "STOP",
			},
		},
	}

	canonResp, err := FromGoogleResponse(googleResp, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("FromGoogleResponse failed: %v", err)
	}

	if len(canonResp.Message.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(canonResp.Message.Parts))
	}
	part := canonResp.Message.Parts[0]
	if part.Type != canonical.PartToolCall {
		t.Fatalf("expected PartToolCall, got %s", part.Type)
	}
	if part.ToolCallID != "call_default_api_Bash_0" {
		t.Errorf("expected sanitized ID 'call_default_api_Bash_0', got %q", part.ToolCallID)
	}
	if part.ThoughtSignature != fakeSig {
		t.Errorf("expected ThoughtSignature %q, got %q", fakeSig, part.ThoughtSignature)
	}

	// 2. Next turn: Claude Code sends back history containing the assistant's tool_use
	// (Claude Code does NOT send ThoughtSignature, only ToolCallID)
	canonReq := &canonical.CanonicalRequest{
		Model: "gemini-2.5-flash",
		Messages: []canonical.Message{
			{
				Role: canonical.RoleUser,
				Parts: []canonical.ContentPart{
					{Type: canonical.PartText, Text: "Check git status"},
				},
			},
			{
				Role: canonical.RoleAssistant,
				Parts: []canonical.ContentPart{
					{
						Type:         canonical.PartToolCall,
						ToolCallID:   "call_default_api_Bash_0",
						ToolCallName: "default_api:Bash",
						ToolCallArgs: `{"command":"git status"}`,
						// ThoughtSignature is intentionally empty here to simulate Claude Code!
					},
				},
			},
			{
				Role: canonical.RoleTool,
				Parts: []canonical.ContentPart{
					{
						Type:              canonical.PartToolResult,
						ToolResultID:      "call_default_api_Bash_0",
						ToolResultContent: "On branch main\nnothing to commit",
					},
				},
			},
		},
	}

	// Convert to Google request
	googleReq, err := ToGoogleRequest(canonReq)
	if err != nil {
		t.Fatalf("ToGoogleRequest failed: %v", err)
	}

	// Verify the assistant turn's functionCall part recovered the authentic thought_signature
	if len(googleReq.Contents) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(googleReq.Contents))
	}
	modelTurn := googleReq.Contents[1]
	if modelTurn.Role != "model" {
		t.Fatalf("expected model role, got %s", modelTurn.Role)
	}
	if len(modelTurn.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(modelTurn.Parts))
	}
	modelPart := modelTurn.Parts[0]
	if modelPart.FunctionCall == nil || modelPart.FunctionCall.Name != "default_api:Bash" {
		t.Errorf("expected FunctionCall 'default_api:Bash', got %+v", modelPart.FunctionCall)
	}
	if modelPart.ThoughtSignature != fakeSig {
		t.Errorf("expected restored ThoughtSignature %q, got %q", fakeSig, modelPart.ThoughtSignature)
	}
}

func TestGoogleThoughtSignature_FallbackSentinel(t *testing.T) {
	// A tool call that was never seen before (e.g. from Claude or GPT model)
	canonReq := &canonical.CanonicalRequest{
		Model: "gemini-2.5-flash",
		Messages: []canonical.Message{
			{
				Role: canonical.RoleUser,
				Parts: []canonical.ContentPart{
					{Type: canonical.PartText, Text: "List files"},
				},
			},
			{
				Role: canonical.RoleAssistant,
				Parts: []canonical.ContentPart{
					{
						Type:         canonical.PartToolCall,
						ToolCallID:   "unknown_call_9999",
						ToolCallName: "default_api:Bash",
						ToolCallArgs: `{"command":"ls"}`,
					},
				},
			},
			{
				Role: canonical.RoleTool,
				Parts: []canonical.ContentPart{
					{
						Type:              canonical.PartToolResult,
						ToolResultID:      "unknown_call_9999",
						ToolResultContent: "file1.txt\nfile2.txt",
					},
				},
			},
		},
	}

	googleReq, err := ToGoogleRequest(canonReq)
	if err != nil {
		t.Fatalf("ToGoogleRequest failed: %v", err)
	}

	if len(googleReq.Contents) != 3 {
		t.Fatalf("expected 3 contents, got %d: %+v", len(googleReq.Contents), googleReq.Contents)
	}

	part := googleReq.Contents[1].Parts[0]
	if part.ThoughtSignature != "skip_thought_signature_validator" {
		t.Errorf("expected sentinel 'skip_thought_signature_validator', got %q", part.ThoughtSignature)
	}
}

func TestGoogleStreamEvent_ThoughtSignatureAndThinking(t *testing.T) {
	rawChunk := []byte(`{
		"candidates": [
			{
				"content": {
					"parts": [
						{
							"text": "Thinking about the command...",
							"thought": true,
							"thought_signature": "sig_thought_1"
						},
						{
							"functionCall": {
								"name": "default_api:Bash",
								"args": { "command": "pwd" }
							},
							"thought_signature": "sig_fn_1"
						}
					]
				},
				"index": 0
			}
		]
	}`)

	events, err := ParseGoogleStreamEvent(rawChunk, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("ParseGoogleStreamEvent failed: %v", err)
	}

	// Should have: EventThinkingDelta, EventToolCallStart, EventToolCallDelta, EventToolCallDone
	hasThinking := false
	hasToolCall := false
	for _, ev := range events {
		if ev.Type == canonical.EventThinkingDelta {
			hasThinking = true
			if ev.Thinking != "Thinking about the command..." {
				t.Errorf("unexpected thinking text: %q", ev.Thinking)
			}
		}
		if ev.Type == canonical.EventToolCallStart {
			hasToolCall = true
			if ev.ToolCallName != "default_api:Bash" {
				t.Errorf("unexpected tool name: %q", ev.ToolCallName)
			}
			if ev.ThoughtSignature != "sig_fn_1" {
				t.Errorf("unexpected signature: %q", ev.ThoughtSignature)
			}
		}
	}

	if !hasThinking {
		t.Errorf("expected EventThinkingDelta, but none found")
	}
	if !hasToolCall {
		t.Errorf("expected EventToolCallStart, but none found")
	}
}

func TestGooglePart_JSONUnmarshalVariants(t *testing.T) {
	snakeJSON := []byte(`{"functionCall":{"name":"test"},"thought_signature":"sig_snake"}`)
	var pSnake Part
	if err := json.Unmarshal(snakeJSON, &pSnake); err != nil {
		t.Fatalf("unmarshal snake failed: %v", err)
	}
	if pSnake.ThoughtSignature != "sig_snake" {
		t.Errorf("expected 'sig_snake', got %q", pSnake.ThoughtSignature)
	}

	camelJSON := []byte(`{"functionCall":{"name":"test"},"thoughtSignature":"sig_camel"}`)
	var pCamel Part
	if err := json.Unmarshal(camelJSON, &pCamel); err != nil {
		t.Fatalf("unmarshal camel failed: %v", err)
	}
	if pCamel.ThoughtSignature != "sig_camel" {
		t.Errorf("expected 'sig_camel', got %q", pCamel.ThoughtSignature)
	}
}

func TestGoogleToRequest_TrailingModelTurnNormalized(t *testing.T) {
	// Request ending with an assistant prefill turn
	canonReq := &canonical.CanonicalRequest{
		Model: "gemini-3.7-flash",
		Messages: []canonical.Message{
			{
				Role: canonical.RoleUser,
				Parts: []canonical.ContentPart{
					{Type: canonical.PartText, Text: "Say hello"},
				},
			},
			{
				Role: canonical.RoleAssistant,
				Parts: []canonical.ContentPart{
					{Type: canonical.PartText, Text: "Hello, world!"},
				},
			},
		},
	}

	googleReq, err := ToGoogleRequest(canonReq)
	if err != nil {
		t.Fatalf("ToGoogleRequest failed: %v", err)
	}

	if len(googleReq.Contents) != 3 {
		t.Fatalf("expected 3 contents (user, model, user), got %d: %+v", len(googleReq.Contents), googleReq.Contents)
	}

	last := googleReq.Contents[len(googleReq.Contents)-1]
	if last.Role != "user" {
		t.Errorf("expected last turn to be 'user', got %q", last.Role)
	}
	if len(last.Parts) == 0 || last.Parts[0].Text != "Continue" {
		t.Errorf("expected 'Continue' user part, got %+v", last.Parts)
	}
}

func TestClaudeCodeScenario_ToolSequence(t *testing.T) {
	// Simulate Claude Code tool_use followed by user tool_result
	canonReq := &canonical.CanonicalRequest{
		Model: "gemini-3.7-flash",
		Messages: []canonical.Message{
			{
				Role: canonical.RoleUser,
				Parts: []canonical.ContentPart{
					{Type: canonical.PartText, Text: "do we have open changes here?"},
				},
			},
			{
				Role: canonical.RoleAssistant,
				Parts: []canonical.ContentPart{
					{
						Type:         canonical.PartToolCall,
						ToolCallID:   "call_1299952",
						ToolCallName: "Bash",
						ToolCallArgs: `{"command":"git status"}`,
					},
				},
			},
			{
				Role: canonical.RoleTool,
				Parts: []canonical.ContentPart{
					{
						Type:              canonical.PartToolResult,
						ToolResultID:      "call_1299952",
						ToolResultContent: "On branch main\nUntracked files...",
					},
				},
			},
		},
	}

	googleReq, err := ToGoogleRequest(canonReq)
	if err != nil {
		t.Fatalf("ToGoogleRequest failed: %v", err)
	}

	b, _ := json.MarshalIndent(googleReq, "", "  ")
	t.Logf("Generated Google Request:\n%s", string(b))

	last := googleReq.Contents[len(googleReq.Contents)-1]
	if last.Role != "user" {
		t.Errorf("expected last turn to be 'user', got %q", last.Role)
	}
	if len(last.Parts) == 0 || last.Parts[0].FunctionResponse == nil {
		t.Errorf("expected last part to be FunctionResponse, got %+v", last.Parts)
	}
}




