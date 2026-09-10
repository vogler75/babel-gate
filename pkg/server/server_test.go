package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/metrics"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server/trace"
)

type mockUpstreamGoogle struct{}

func (m *mockUpstreamGoogle) Name() string     { return "google" }
func (m *mockUpstreamGoogle) Type() string     { return "google" }
func (m *mockUpstreamGoogle) Endpoint() string { return "https://mock.google.com" }

func (m *mockUpstreamGoogle) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	return &canonical.CanonicalResponse{
		ID:    "gemini-test-id",
		Model: req.Model,
		Message: canonical.Message{
			Role: canonical.RoleAssistant,
			Parts: []canonical.ContentPart{
				{Type: canonical.PartText, Text: "Response from Gemini for prompt: " + req.Messages[len(req.Messages)-1].TextContent()},
			},
		},
		FinishReason: "stop",
		Usage: canonical.Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}, nil
}

func (m *mockUpstreamGoogle) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	ch := make(chan canonical.CanonicalEvent, 8)
	go func() {
		defer close(ch)
		ch <- canonical.CanonicalEvent{Type: canonical.EventMessageStart, MessageID: "msg-stream-1", Model: req.Model}
		ch <- canonical.CanonicalEvent{Type: canonical.EventTextDelta, Text: "Hello from ", Model: req.Model}
		ch <- canonical.CanonicalEvent{Type: canonical.EventTextDelta, Text: "Gemini stream!", Model: req.Model}
		ch <- canonical.CanonicalEvent{
			Type:         canonical.EventMessageDelta,
			FinishReason: "stop",
			Usage:        &canonical.Usage{CompletionTokens: 5},
		}
		ch <- canonical.CanonicalEvent{Type: canonical.EventMessageDone}
	}()
	return ch, nil
}

func (m *mockUpstreamGoogle) ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	return []providers.ModelInfo{
		{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro"},
		{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash"},
	}, nil
}

type mockUpstreamOpenAI struct{}

func (m *mockUpstreamOpenAI) Name() string     { return "openai" }
func (m *mockUpstreamOpenAI) Type() string     { return "openai" }
func (m *mockUpstreamOpenAI) Endpoint() string { return "https://mock.openai.com" }

func (m *mockUpstreamOpenAI) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	return &canonical.CanonicalResponse{
		ID:    "chatcmpl-test",
		Model: req.Model,
		Message: canonical.Message{
			Role: canonical.RoleAssistant,
			Parts: []canonical.ContentPart{
				{Type: canonical.PartText, Text: "Response from OpenAI"},
			},
		},
		FinishReason: "stop",
	}, nil
}
func (m *mockUpstreamOpenAI) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	ch := make(chan canonical.CanonicalEvent, 2)
	ch <- canonical.CanonicalEvent{Type: canonical.EventTextDelta, Text: "Stream from OpenAI"}
	ch <- canonical.CanonicalEvent{Type: canonical.EventMessageDone}
	close(ch)
	return ch, nil
}
func (m *mockUpstreamOpenAI) ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	return []providers.ModelInfo{
		{ID: "gpt-4o", Name: "GPT-4o"},
	}, nil
}

func setupTestHandler() http.Handler {
	tmpFile, _ := os.CreateTemp("", "babelgate_test_metrics_*.db")
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	_ = os.Remove(tmpPath)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:           8080,
			TimeoutSeconds: 30,
			CORSOrigins:    []string{"*"},
		},
		Database: config.DatabaseConfig{
			Path:          tmpPath,
			RetentionDays: 90,
		},
		Routing: config.RoutingConfig{
			Routes: map[string]string{
				// Claude Code asks for claude-3-7-sonnet -> route to Google Gemini
				"claude-3-7-sonnet": "google/gemini-2.5-pro",
			},
		},
	}

	engine, _ := router.NewEngine(cfg)
	engine.RegisterProvider(&mockUpstreamGoogle{})
	engine.RegisterProvider(&mockUpstreamOpenAI{})

	srv := NewServer(cfg, engine)
	return srv.httpServer.Handler
}

// TestClaudeCodeToGeminiNonStreaming verifies Anthropic /v1/messages routed to Gemini
func TestClaudeCodeToGeminiNonStreaming(t *testing.T) {
	handler := setupTestHandler()

	anthropicPayload := map[string]any{
		"model": "claude-3-7-sonnet", // rewritten by route to google/gemini-2.5-pro
		"messages": []map[string]any{
			{"role": "user", "content": "Hello router!"},
		},
		"max_tokens": 1000,
	}

	b, _ := json.Marshal(anthropicPayload)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var anthResp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&anthResp); err != nil {
		t.Fatalf("json decode error: %v", err)
	}

	// Verify Anthropic format response
	if anthResp["type"] != "message" || anthResp["role"] != "assistant" {
		t.Errorf("unexpected anthropic response wrapper: %+v", anthResp)
	}

	content := anthResp["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content blocks in response")
	}
	block := content[0].(map[string]any)
	text := block["text"].(string)
	if !strings.Contains(text, "Response from Gemini") {
		t.Errorf("expected Gemini response content, got %s", text)
	}
}

// TestClaudeCodeToGeminiStreaming verifies Anthropic SSE events emitted from Gemini upstream
func TestClaudeCodeToGeminiStreaming(t *testing.T) {
	handler := setupTestHandler()

	anthropicPayload := map[string]any{
		"model": "claude-3-7-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": "Stream test"},
		},
		"max_tokens": 1000,
		"stream":     true,
	}

	b, _ := json.Marshal(anthropicPayload)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	scanner := bufio.NewScanner(rec.Body)
	eventsFound := make(map[string]bool)
	var accumulatedText string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventName := strings.TrimPrefix(line, "event: ")
			eventsFound[eventName] = true
		}
		if strings.HasPrefix(line, "data: ") {
			dataStr := strings.TrimPrefix(line, "data: ")
			var chunk map[string]any
			if err := json.Unmarshal([]byte(dataStr), &chunk); err == nil {
				if delta, ok := chunk["delta"].(map[string]any); ok {
					if txt, ok := delta["text"].(string); ok {
						accumulatedText += txt
					}
				}
			}
		}
	}

	// Verify required Anthropic events received
	if !eventsFound["message_start"] {
		t.Errorf("missing message_start event")
	}
	if !eventsFound["content_block_start"] {
		t.Errorf("missing content_block_start event")
	}
	if !eventsFound["content_block_delta"] {
		t.Errorf("missing content_block_delta event")
	}
	if !eventsFound["content_block_stop"] {
		t.Errorf("missing content_block_stop event")
	}
	if !eventsFound["message_delta"] {
		t.Errorf("missing message_delta event")
	}
	if !eventsFound["message_stop"] {
		t.Errorf("missing message_stop event")
	}

	if accumulatedText != "Hello from Gemini stream!" {
		t.Errorf("expected accumulated text 'Hello from Gemini stream!', got %q", accumulatedText)
	}
}

// TestOpenAIClientToUpstream verifies OpenAI /v1/chat/completions routed to upstream
func TestOpenAIClientToUpstream(t *testing.T) {
	handler := setupTestHandler()

	payload := map[string]any{
		"model": "google/gemini-2.5-pro",
		"messages": []map[string]any{
			{"role": "user", "content": "OpenAI test"},
		},
	}

	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var oaiResp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&oaiResp)
	choices := oaiResp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices returned")
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if !strings.Contains(msg["content"].(string), "Response from Gemini") {
		t.Errorf("unexpected content: %v", msg["content"])
	}
}

// TestGoogleClientToUpstream verifies Google Gemini /v1beta/models/...:generateContent
func TestGoogleClientToUpstream(t *testing.T) {
	handler := setupTestHandler()

	payload := map[string]any{
		"contents": []map[string]any{
			{
				"role": "user",
				"parts": []map[string]any{
					{"text": "Gemini API test"},
				},
			},
		},
	}

	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/openai/gpt-4o:generateContent", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var googResp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&googResp)
	candidates := googResp["candidates"].([]any)
	if len(candidates) == 0 {
		t.Fatalf("no candidates returned")
	}
	cand := candidates[0].(map[string]any)
	content := cand["content"].(map[string]any)
	parts := content["parts"].([]any)
	p0 := parts[0].(map[string]any)
	if p0["text"] != "Response from OpenAI" {
		t.Errorf("unexpected part text: %v", p0["text"])
	}
}

// TestModelsCatalogEndpoints verifies /v1/models, /v1beta/models, /api/status, /api/models
func TestModelsCatalogEndpoints(t *testing.T) {
	handler := setupTestHandler()

	// 1. OpenAI /v1/models
	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("/v1/models failed: %d", rec1.Code)
	}
	var oaiModels map[string]any
	_ = json.NewDecoder(rec1.Body).Decode(&oaiModels)
	if oaiModels["object"] != "list" {
		t.Errorf("expected object list, got %v", oaiModels["object"])
	}

	// 2. Google /v1beta/models
	req2 := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("/v1beta/models failed: %d", rec2.Code)
	}
	var googModels map[string]any
	_ = json.NewDecoder(rec2.Body).Decode(&googModels)
	if googModels["models"] == nil {
		t.Errorf("expected models field in google catalog")
	}

	// 3. Status API /api/status
	req3 := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("/api/status failed: %d", rec3.Code)
	}
	var status map[string]any
	_ = json.NewDecoder(rec3.Body).Decode(&status)
	if status["status"] != "healthy" {
		t.Errorf("expected status healthy, got %v", status["status"])
	}

	// 4. Web Dashboard Index /
	req4 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Fatalf("dashboard index / failed: %d", rec4.Code)
	}
}

// TestAnthropicClientSeesGeminiModelsAndCanCallDirectly verifies:
// 1. Anthropic client querying /v1/models sees Gemini models from the Gemini connector
// 2. Anthropic client can call POST /v1/messages specifying "gemini-2.5-pro" directly
func TestAnthropicClientSeesGeminiModelsAndCanCallDirectly(t *testing.T) {
	handler := setupTestHandler()

	// 1. Query models using Anthropic client headers
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "anthropic-client-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /v1/models, got %d: %s", rec.Code, rec.Body.String())
	}

	var anthModels map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&anthModels); err != nil {
		t.Fatalf("decode anthropic models error: %v", err)
	}

	data, ok := anthModels["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("expected non-empty data array in anthropic models response, got: %+v", anthModels)
	}

	// Verify that Gemini models from the Google connector are listed
	foundGemini := false
	for _, item := range data {
		mMap := item.(map[string]any)
		id := mMap["id"].(string)
		if id == "gemini-2.5-pro" || id == "google/gemini-2.5-pro" {
			foundGemini = true
			break
		}
	}
	if !foundGemini {
		t.Errorf("expected Gemini connector model in Anthropic models list, but none found: %+v", data)
	}

	// 2. Call POST /v1/messages directly with model: "gemini-2.5-pro" (without any alias)
	msgPayload := map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello Gemini from Anthropic client!"},
		},
		"max_tokens": 100,
	}

	b, _ := json.Marshal(msgPayload)
	msgReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(b))
	msgReq.Header.Set("Content-Type", "application/json")
	msgReq.Header.Set("x-api-key", "test-key")

	msgRec := httptest.NewRecorder()
	handler.ServeHTTP(msgRec, msgReq)

	if msgRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK calling Gemini via Anthropic endpoint, got %d: %s", msgRec.Code, msgRec.Body.String())
	}

	var msgResp map[string]any
	if err := json.NewDecoder(msgRec.Body).Decode(&msgResp); err != nil {
		t.Fatalf("decode msg error: %v", err)
	}

	if msgResp["type"] != "message" {
		t.Errorf("expected Anthropic message response wrapper, got: %+v", msgResp)
	}
	content := msgResp["content"].([]any)
	block := content[0].(map[string]any)
	text := block["text"].(string)
	if !strings.Contains(text, "Response from Gemini") {
		t.Errorf("expected response from Gemini, got %s", text)
	}
}

func TestSessionsAPIAndTokenTracking(t *testing.T) {
	handler := setupTestHandler()

	// 1. Initial /api/sessions check: should be 0 sessions
	req0 := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	rec0 := httptest.NewRecorder()
	handler.ServeHTTP(rec0, req0)
	if rec0.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/sessions, got %d", rec0.Code)
	}

	// 2. Perform Anthropic request with Claude Code User-Agent
	claudePayload := map[string]any{
		"model": "claude-3-7-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": "Explain router architecture"},
		},
		"max_tokens": 500,
	}
	bClaude, _ := json.Marshal(claudePayload)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(bClaude))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("User-Agent", "claude-code/0.2.29 darwin-arm64")
	req1.Header.Set("X-Session-ID", "sess_claude_123")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from Claude request, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 3. Perform OpenAI request with Web Playground client
	oaiPayload := map[string]any{
		"model": "gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello from playground"},
		},
	}
	bOAI, _ := json.Marshal(oaiPayload)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bOAI))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Client", "Web Playground")
	req2.Header.Set("X-Session-ID", "sess_play_456")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from Playground request, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// 4. Query /api/sessions
	reqSessions := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	recSessions := httptest.NewRecorder()
	handler.ServeHTTP(recSessions, reqSessions)

	if recSessions.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/sessions, got %d", recSessions.Code)
	}

	var sessResp struct {
		Summary struct {
			TotalSessions     int `json:"total_sessions"`
			TotalRequests     int `json:"total_requests"`
			TotalInputTokens  int `json:"total_input_tokens"`
			TotalOutputTokens int `json:"total_output_tokens"`
			TotalTokens       int `json:"total_tokens"`
		} `json:"summary"`
		Sessions []struct {
			ID           string   `json:"id"`
			Client       string   `json:"client"`
			RequestCount int      `json:"request_count"`
			InputTokens  int      `json:"input_tokens"`
			OutputTokens int      `json:"output_tokens"`
			TotalTokens  int      `json:"total_tokens"`
			LastModel    string   `json:"last_model"`
			Models       []string `json:"models"`
			ModelStats   map[string]struct {
				Model        string  `json:"model"`
				RequestCount int     `json:"request_count"`
				PercentReq   float64 `json:"percent_req"`
			} `json:"model_stats"`
		} `json:"sessions"`
	}

	if err := json.NewDecoder(recSessions.Body).Decode(&sessResp); err != nil {
		t.Fatalf("failed to parse /api/sessions json: %v", err)
	}

	if sessResp.Summary.TotalSessions != 2 {
		t.Errorf("expected 2 total sessions, got %d", sessResp.Summary.TotalSessions)
	}
	if sessResp.Summary.TotalRequests != 2 {
		t.Errorf("expected 2 total requests, got %d", sessResp.Summary.TotalRequests)
	}
	if sessResp.Summary.TotalInputTokens <= 0 {
		t.Errorf("expected positive input tokens, got %d", sessResp.Summary.TotalInputTokens)
	}
	if sessResp.Summary.TotalOutputTokens <= 0 {
		t.Errorf("expected positive output tokens, got %d", sessResp.Summary.TotalOutputTokens)
	}
	if sessResp.Summary.TotalTokens != sessResp.Summary.TotalInputTokens+sessResp.Summary.TotalOutputTokens {
		t.Errorf("total tokens mismatch: %d != %d + %d", sessResp.Summary.TotalTokens, sessResp.Summary.TotalInputTokens, sessResp.Summary.TotalOutputTokens)
	}

	for _, s := range sessResp.Sessions {
		if s.ID == "sess_claude_123" {
			if s.LastModel != "google/gemini-2.5-pro" {
				t.Errorf("expected last model 'google/gemini-2.5-pro', got %q", s.LastModel)
			}
			if len(s.ModelStats) == 0 || s.ModelStats["google/gemini-2.5-pro"].PercentReq != 100 {
				t.Errorf("expected 100%% percent req for gemini, got %+v", s.ModelStats)
			}
			if len(s.Models) == 0 || s.Models[0] != "google/gemini-2.5-pro" {
				t.Errorf("expected 'google/gemini-2.5-pro' in claude session models, got: %+v", s.Models)
			}
		}
		if s.ID == "sess_play_456" {
			if s.LastModel != "openai/gpt-4o" {
				t.Errorf("expected last model 'openai/gpt-4o', got %q", s.LastModel)
			}
			if len(s.ModelStats) == 0 || s.ModelStats["openai/gpt-4o"].PercentReq != 100 {
				t.Errorf("expected 100%% percent req for gpt-4o, got %+v", s.ModelStats)
			}
			if len(s.Models) == 0 || s.Models[0] != "openai/gpt-4o" {
				t.Errorf("expected 'openai/gpt-4o' in playground session models, got: %+v", s.Models)
			}
		}
	}

	// 5. Test Clear endpoint
	reqClear := httptest.NewRequest(http.MethodPost, "/api/sessions/clear", nil)
	recClear := httptest.NewRecorder()
	handler.ServeHTTP(recClear, reqClear)
	if recClear.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from clear, got %d", recClear.Code)
	}

	// Check /api/sessions after clear
	recAfterClear := httptest.NewRecorder()
	handler.ServeHTTP(recAfterClear, reqSessions)
	var afterResp struct {
		Summary struct {
			TotalSessions int `json:"total_sessions"`
		} `json:"summary"`
	}
	_ = json.NewDecoder(recAfterClear.Body).Decode(&afterResp)
	if afterResp.Summary.TotalSessions != 0 {
		t.Errorf("expected 0 sessions after clear, got %d", afterResp.Summary.TotalSessions)
	}
}

func TestDashboardAndSetupHelpPages(t *testing.T) {
	handler := setupTestHandler()

	// 1. Check Dashboard GET /
	reqIndex := httptest.NewRequest(http.MethodGet, "/", nil)
	recIndex := httptest.NewRecorder()
	handler.ServeHTTP(recIndex, reqIndex)
	if recIndex.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /, got %d", recIndex.Code)
	}
	bodyIndex := recIndex.Body.String()
	if !strings.Contains(bodyIndex, "Setup Help") {
		t.Errorf("expected / to contain 'Setup Help' button")
	}
	if !strings.Contains(bodyIndex, "/setup") {
		t.Errorf("expected / to link to '/setup'")
	}

	// 2. Check Setup page GET /setup
	reqSetup := httptest.NewRequest(http.MethodGet, "/setup", nil)
	recSetup := httptest.NewRecorder()
	handler.ServeHTTP(recSetup, reqSetup)
	if recSetup.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /setup, got %d", recSetup.Code)
	}
	bodySetup := recSetup.Body.String()

	expectedPhrases := []string{
		"Claude Code Setup",
		"ANTHROPIC_BASE_URL",
		"Codex CLI",
		"OPENAI_BASE_URL",
		"openai_base_url",
		"Antigravity CLI",
		"GOOGLE_GEMINI_BASE_URL",
		"modelProvider",
		"Back to Dashboard",
	}
	for _, phrase := range expectedPhrases {
		if !strings.Contains(bodySetup, phrase) {
			t.Errorf("expected /setup to contain %q", phrase)
		}
	}
}

func TestMetricsAPIEndToEnd(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := tempDir + "/test_metrics.db"

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:           8080,
			TimeoutSeconds: 30,
			CORSOrigins:    []string{"*"},
		},
		Database: config.DatabaseConfig{
			Path:          dbPath,
			RetentionDays: 90,
		},
		Routing: config.RoutingConfig{
			Routes: map[string]string{
				"claude-3-7-sonnet": "google/gemini-2.5-pro",
			},
		},
	}

	engine, _ := router.NewEngine(cfg)
	engine.RegisterProvider(&mockUpstreamGoogle{})
	engine.RegisterProvider(&mockUpstreamOpenAI{})

	srv := NewServer(cfg, engine)
	defer srv.Shutdown(context.Background())
	handler := srv.httpServer.Handler

	// 1. Send request through /v1/messages (mapped to Google)
	msgPayload := map[string]any{
		"model": "claude-3-7-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello metrics test"},
		},
		"max_tokens": 100,
	}
	bMsg, _ := json.Marshal(msgPayload)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(bMsg))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /v1/messages, got %d", rec1.Code)
	}

	// 2. Query /api/metrics/summary
	reqSum := httptest.NewRequest(http.MethodGet, "/api/metrics/summary", nil)
	recSum := httptest.NewRecorder()
	handler.ServeHTTP(recSum, reqSum)
	if recSum.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/metrics/summary, got %d", recSum.Code)
	}

	var sum metrics.MetricsSummary
	if err := json.NewDecoder(recSum.Body).Decode(&sum); err != nil {
		t.Fatalf("failed to decode metrics summary: %v", err)
	}
	if sum.Requests != 1 {
		t.Errorf("expected 1 request in summary, got %d", sum.Requests)
	}
	if sum.TotalTokens != 30 {
		t.Errorf("expected 30 total tokens in summary, got %d", sum.TotalTokens)
	}
	if len(sum.TopModels) == 0 {
		t.Fatalf("expected at least 1 top model in summary")
	}
	if sum.TopModels[0].Provider != "google" {
		t.Errorf("expected provider 'google', got %q", sum.TopModels[0].Provider)
	}

	// 3. Query /api/metrics/daily
	reqDaily := httptest.NewRequest(http.MethodGet, "/api/metrics/daily", nil)
	recDaily := httptest.NewRecorder()
	handler.ServeHTTP(recDaily, reqDaily)
	if recDaily.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/metrics/daily, got %d", recDaily.Code)
	}

	// 4. Query /api/metrics/hourly for today
	todayStr := time.Now().UTC().Format("2006-01-02")
	reqHourly := httptest.NewRequest(http.MethodGet, "/api/metrics/hourly?date="+todayStr, nil)
	recHourly := httptest.NewRecorder()
	handler.ServeHTTP(recHourly, reqHourly)
	if recHourly.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/metrics/hourly, got %d", recHourly.Code)
	}
}

func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origWriter)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})

	handler := loggingMiddleware(next)
	req := httptest.NewRequest(http.MethodPost, "/test/path", nil)
	req.Header.Set("x-client", "claude-code")
	req.Header.Set("x-session-id", "test-session-123456789")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", rec.Code)
	}

	logged := buf.String()
	if !strings.Contains(logged, "[POST] /test/path -> 201") {
		t.Errorf("expected log to contain method, path, and status code, got: %s", logged)
	}
	if !strings.Contains(logged, "client: claude-code") {
		t.Errorf("expected log to contain client info, got: %s", logged)
	}
	if !strings.Contains(logged, "sess: test-session…") {
		t.Errorf("expected log to contain truncated session ID, got: %s", logged)
	}
}

func TestLoggingMiddlewareSkipsDashboardAPI(t *testing.T) {
	var buf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origWriter)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := loggingMiddleware(next)
	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if logged := buf.String(); logged != "" {
		t.Errorf("expected dashboard API request not to be logged, got: %s", logged)
	}
}

func TestLoggingMiddlewareWithTrace(t *testing.T) {
	var buf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origWriter)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr := trace.FromContext(r.Context())
		if tr != nil {
			tr.SetRoute("claude-3-7-sonnet", "copilot", "https://api.githubcopilot.com", "claude-3-7-sonnet")
			tr.SetReadDuration(12 * time.Millisecond)
			tr.MarkFirstToken()
			tr.SetTokens(1500, 320)
			tr.MarkStreamDone()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	handler := loggingMiddleware(next)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logged := buf.String()
	if !strings.Contains(logged, "[claude-3-7-sonnet via copilot -> https://api.githubcopilot.com: 1,500 in / 320 out]") {
		t.Errorf("expected log to contain route and destination, got: %s", logged)
	}
	if !strings.Contains(logged, "read: 12ms") {
		t.Errorf("expected log to contain read duration, got: %s", logged)
	}
	if !strings.Contains(logged, "ttft:") {
		t.Errorf("expected log to contain ttft, got: %s", logged)
	}
	if !strings.Contains(logged, "stream:") {
		t.Errorf("expected log to contain stream duration, got: %s", logged)
	}
}

func TestServerRestartSessionContinuity(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "restart_test_metrics.db")

	buildCfg := func() *config.Config {
		return &config.Config{
			Server: config.ServerConfig{
				Port:           8080,
				TimeoutSeconds: 30,
				CORSOrigins:    []string{"*"},
			},
			Database: config.DatabaseConfig{
				Path:          dbPath,
				RetentionDays: 90,
			},
			Routing: config.RoutingConfig{
				Routes: map[string]string{
					"claude-3-7-sonnet": "google/gemini-2.5-pro",
				},
			},
		}
	}

	// --- Instance 1: Before restart ---
	cfg1 := buildCfg()
	engine1, _ := router.NewEngine(cfg1)
	engine1.RegisterProvider(&mockUpstreamGoogle{})
	srv1 := NewServer(cfg1, engine1)

	// Send request 1 with specific session ID
	msgPayload := map[string]any{
		"model": "claude-3-7-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": "Turn 1"},
		},
	}
	body1, _ := json.Marshal(msgPayload)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Session-ID", "claude-code-sess-persisted")
	req1.Header.Set("User-Agent", "claude-code/1.0")
	rec1 := httptest.NewRecorder()

	srv1.httpServer.Handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("instance 1 request failed: %d: %s", rec1.Code, rec1.Body.String())
	}

	// Verify session on instance 1
	sessReq1 := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	sessRec1 := httptest.NewRecorder()
	srv1.httpServer.Handler.ServeHTTP(sessRec1, sessReq1)

	var sessResp1 struct {
		Summary struct {
			TotalSessions int `json:"total_sessions"`
			TotalRequests int `json:"total_requests"`
			TotalTokens   int `json:"total_tokens"`
		} `json:"summary"`
		Sessions []struct {
			ID           string `json:"id"`
			RequestCount int    `json:"request_count"`
			TotalTokens  int    `json:"total_tokens"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(sessRec1.Body).Decode(&sessResp1); err != nil {
		t.Fatalf("failed to decode sessions response: %v", err)
	}
	if sessResp1.Summary.TotalSessions != 1 || sessResp1.Summary.TotalRequests != 1 {
		t.Fatalf("expected 1 session and 1 request on instance 1, got %d sessions, %d requests",
			sessResp1.Summary.TotalSessions, sessResp1.Summary.TotalRequests)
	}

	// Shut down instance 1 cleanly
	_ = srv1.Shutdown(context.Background())

	// --- Instance 2: Fresh instance pointing to the same SQLite database ---
	cfg2 := buildCfg()
	engine2, _ := router.NewEngine(cfg2)
	engine2.RegisterProvider(&mockUpstreamGoogle{})
	srv2 := NewServer(cfg2, engine2)
	defer srv2.Shutdown(context.Background())

	// Query /api/sessions immediately on instance 2 (before any new request)
	sessReq2 := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	sessRec2 := httptest.NewRecorder()
	srv2.httpServer.Handler.ServeHTTP(sessRec2, sessReq2)

	var sessResp2 struct {
		Summary struct {
			TotalSessions int `json:"total_sessions"`
			TotalRequests int `json:"total_requests"`
			TotalTokens   int `json:"total_tokens"`
		} `json:"summary"`
		Sessions []struct {
			ID             string `json:"id"`
			Client         string `json:"client"`
			RequestCount   int    `json:"request_count"`
			TotalTokens    int    `json:"total_tokens"`
			RecentRequests []any  `json:"recent_requests"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(sessRec2.Body).Decode(&sessResp2); err != nil {
		t.Fatalf("failed to decode instance 2 sessions response: %v", err)
	}

	if sessResp2.Summary.TotalSessions != 1 {
		t.Fatalf("expected 1 session restored on instance 2, got %d", sessResp2.Summary.TotalSessions)
	}
	if sessResp2.Summary.TotalRequests != 1 {
		t.Fatalf("expected 1 request restored on instance 2, got %d", sessResp2.Summary.TotalRequests)
	}
	if len(sessResp2.Sessions) != 1 || sessResp2.Sessions[0].ID != "claude-code-sess-persisted" {
		t.Fatalf("expected restored session 'claude-code-sess-persisted', got %+v", sessResp2.Sessions)
	}
	if len(sessResp2.Sessions[0].RecentRequests) != 1 {
		t.Fatalf("expected 1 recent request restored, got %d", len(sessResp2.Sessions[0].RecentRequests))
	}

	// --- Client sends Turn 2 after restart with the SAME session ID ---
	msgPayload2 := map[string]any{
		"model": "claude-3-7-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": "Turn 1"},
			{"role": "assistant", "content": "Response from turn 1"},
			{"role": "user", "content": "Turn 2"},
		},
	}
	body2, _ := json.Marshal(msgPayload2)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Session-ID", "claude-code-sess-persisted")
	req2.Header.Set("User-Agent", "claude-code/1.0")
	rec2 := httptest.NewRecorder()

	srv2.httpServer.Handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("instance 2 turn 2 request failed: %d: %s", rec2.Code, rec2.Body.String())
	}

	// Query /api/sessions on instance 2 after turn 2
	sessReq3 := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	sessRec3 := httptest.NewRecorder()
	srv2.httpServer.Handler.ServeHTTP(sessRec3, sessReq3)

	var sessResp3 struct {
		Summary struct {
			TotalSessions int `json:"total_sessions"`
			TotalRequests int `json:"total_requests"`
			TotalTokens   int `json:"total_tokens"`
		} `json:"summary"`
		Sessions []struct {
			ID             string `json:"id"`
			RequestCount   int    `json:"request_count"`
			TotalTokens    int    `json:"total_tokens"`
			RecentRequests []any  `json:"recent_requests"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(sessRec3.Body).Decode(&sessResp3); err != nil {
		t.Fatalf("failed to decode sessions response: %v", err)
	}

	if sessResp3.Summary.TotalSessions != 1 {
		t.Errorf("expected still 1 session, got %d", sessResp3.Summary.TotalSessions)
	}
	if sessResp3.Summary.TotalRequests != 2 {
		t.Errorf("expected 2 total requests after turn 2, got %d", sessResp3.Summary.TotalRequests)
	}
	if sessResp3.Sessions[0].RequestCount != 2 {
		t.Errorf("expected session request count 2, got %d", sessResp3.Sessions[0].RequestCount)
	}
	if len(sessResp3.Sessions[0].RecentRequests) != 2 {
		t.Errorf("expected 2 recent requests, got %d", len(sessResp3.Sessions[0].RecentRequests))
	}
}
