package inbound

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/session"
)

func TestAnthropicHandler_Passthrough(t *testing.T) {
	// Upstream mock server simulating Anthropic /v1/messages
	upstreamReceivedHeader := ""
	upstreamReceivedBody := ""
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamReceivedHeader = r.Header.Get("anthropic-version")
		bodyBytes, _ := io.ReadAll(r.Body)
		upstreamReceivedBody = string(bodyBytes)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"test reasoning\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_xyz\"}}\n\n")
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstreamServer.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"anthropic": {
				Type:          "anthropic",
				BaseURL:       upstreamServer.URL + "/v1",
				APIKey:        "test-api-key",
				Priority:      1,
				EnabledModels: []string{"claude-3-7-sonnet"},
			},
		},
	}

	engine, err := router.NewEngine(cfg)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	catalog := router.NewCatalog(engine)
	sessions := session.NewManager()

	handler := NewAnthropicHandler(engine, catalog, sessions)

	clientPayload := `{
		"model": "claude-3-7-sonnet",
		"messages": [
			{
				"role": "user",
				"content": "Hello world"
			}
		],
		"stream": true
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(clientPayload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "client-provided-key")

	rec := httptest.NewRecorder()
	handler.HandleMessages(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	// Verify that exact SSE stream from upstream was forwarded
	if !bytes.Contains(respBody, []byte("signature_delta")) {
		t.Errorf("expected response to contain signature_delta, got: %s", respStr)
	}
	if !bytes.Contains(respBody, []byte("sig_xyz")) {
		t.Errorf("expected response to contain sig_xyz, got: %s", respStr)
	}

	if upstreamReceivedHeader != "2023-06-01" {
		t.Errorf("expected upstream to receive anthropic-version 2023-06-01, got %q", upstreamReceivedHeader)
	}

	if !bytes.Contains([]byte(upstreamReceivedBody), []byte("claude-3-7-sonnet")) {
		t.Errorf("expected upstream body to contain model, got %s", upstreamReceivedBody)
	}

	// Verify session token metrics were captured
	summary := sessions.GetSummary()
	if summary.TotalRequests != 1 {
		t.Errorf("expected 1 session request, got %d", summary.TotalRequests)
	}
}

func TestSanitizeAnthropicPayload(t *testing.T) {
	// 1. Empty thinking with signature -> redacted_thinking with data
	inputWithSig := []byte(`{
		"model": "claude-3-7-sonnet",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": "",
						"signature": "sig_secret_token"
					},
					{
						"type": "text",
						"text": "Hello"
					}
				]
			}
		]
	}`)

	sanitized := sanitizeAnthropicPayload(inputWithSig, "claude-3-7-sonnet", "claude-3-7-sonnet")
	if !bytes.Contains(sanitized, []byte(`"type":"redacted_thinking"`)) {
		t.Errorf("expected redacted_thinking, got: %s", string(sanitized))
	}
	if !bytes.Contains(sanitized, []byte(`"data":"sig_secret_token"`)) {
		t.Errorf("expected data field with signature, got: %s", string(sanitized))
	}

	// 2. Empty thinking WITHOUT signature -> removed from content
	inputWithoutSig := []byte(`{
		"model": "claude-3-7-sonnet",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": ""
					},
					{
						"type": "text",
						"text": "Hello"
					}
				]
			}
		]
	}`)

	sanitizedNoSig := sanitizeAnthropicPayload(inputWithoutSig, "claude-3-7-sonnet", "claude-3-7-sonnet")
	if bytes.Contains(sanitizedNoSig, []byte(`"type":"thinking"`)) {
		t.Errorf("expected thinking block without signature to be removed, got: %s", string(sanitizedNoSig))
	}
	if !bytes.Contains(sanitizedNoSig, []byte(`"text":"Hello"`)) {
		t.Errorf("expected text block to remain, got: %s", string(sanitizedNoSig))
	}
}
