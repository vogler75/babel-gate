package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/session"
)

func newGoogleTestServer(t *testing.T, upstream http.Handler) *Server {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	cfg := &config.Config{
		Server:   config.ServerConfig{TimeoutSeconds: 30},
		Database: config.DatabaseConfig{Path: t.TempDir() + "/metrics.db"},
		Providers: map[string]config.ProviderConfig{
			"google": {Type: "google", BaseURL: upstreamServer.URL, APIKey: "test-key", EnabledModels: []string{"gemini-test"}},
		},
		Routing: config.RoutingConfig{Routes: map[string]string{"claude-test": "google/gemini-test"}},
	}
	engine, err := router.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(cfg, engine)
	t.Cleanup(func() {
		if srv.Metrics() != nil {
			_ = srv.Metrics().Close()
		}
	})
	return srv
}

func TestAnthropicCountTokensRouteUsesGoogle(t *testing.T) {
	srv := newGoogleTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:countTokens" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"totalTokens": 987})
	}))

	sess := srv.Sessions().GetOrCreate("conversation", "127.0.0.1", "", "Claude Code")
	srv.Sessions().RecordRequest(sess.ID, session.RequestRecord{InputTokens: 107500, Status: "success"})
	body := `{"model":"claude-test","system":"Be concise","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("x-session-id", sess.ID)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.InputTokens != 987 {
		t.Fatalf("expected native Gemini count 987, got %d", response.InputTokens)
	}
	sessions := srv.Sessions().ListSessions()
	if len(sessions) != 1 || sessions[0].ContextTokens != 107500 || sessions[0].RequestCount != 1 {
		t.Fatalf("partial token-count probe changed conversation context: %+v", sessions)
	}
}

func TestGeminiContextErrorTranslatedForAnthropicClient(t *testing.T) {
	srv := newGoogleTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"The input token count exceeds the maximum number of tokens allowed 1048576.","status":"INVALID_ARGUMENT"}}`))
	}))

	body := `{"model":"claude-test","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected translated 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("expected JSON error, got %q", contentType)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"type":"invalid_request_error"`)) ||
		!bytes.Contains(rec.Body.Bytes(), []byte("prompt is too long")) ||
		!bytes.Contains(rec.Body.Bytes(), []byte("1048576")) {
		t.Fatalf("unexpected translated error: %s", rec.Body.String())
	}
}
