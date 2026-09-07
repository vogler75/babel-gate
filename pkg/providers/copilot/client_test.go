package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/openai"
)

func TestRequestDeviceCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/device/code" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "invalid content type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-12345",
			"user_code":        "ABCD-1234",
			"verification_uri": "https://github.com/login/device",
			"expires_in":       900,
			"interval":         5,
		})
	}))
	defer server.Close()

	// Direct test of json unmarshaling structure
	client := server.Client()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/login/device/code", nil)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var dcr DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dcr); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if dcr.UserCode != "ABCD-1234" || dcr.DeviceCode != "dev-12345" {
		t.Fatalf("unexpected dcr: %+v", dcr)
	}
}

func TestExchangeCopilotToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "token mock-gh-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "tid=test-copilot-token;exp=999999",
			"expires_at": 999999,
			"endpoints": map[string]string{
				"api": "https://api.individual.githubcopilot.com",
			},
		})
	}))
	defer server.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	req.Header.Set("Authorization", "token mock-gh-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var session CopilotSessionToken
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if session.Token != "tid=test-copilot-token;exp=999999" {
		t.Fatalf("unexpected token: %s", session.Token)
	}
}

func TestCopilotClient_ExecuteAndStream(t *testing.T) {
	// Mock Copilot backend
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer tid=test-session-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if r.Header.Get("Editor-Version") == "" || r.Header.Get("Copilot-Integration-Id") == "" {
			http.Error(w, "missing required copilot headers", http.StatusBadRequest)
			return
		}

		switch r.URL.Path {
		case "/chat/completions":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			isStream, _ := req["stream"].(bool)
			if isStream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)

				flusher, _ := w.(http.Flusher)
				chunk1 := `{"id":"c1","object":"chat.completion.chunk","created":100,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"}}]}`
				chunk2 := `{"id":"c2","object":"chat.completion.chunk","created":101,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":" World"}}]}`
				chunk3 := `{"id":"c3","object":"chat.completion.chunk","created":102,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

				fmt.Fprintf(w, "data: %s\n\n", chunk1)
				flusher.Flush()
				fmt.Fprintf(w, "data: %s\n\n", chunk2)
				flusher.Flush()
				fmt.Fprintf(w, "data: %s\n\n", chunk3)
				flusher.Flush()
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}

			// Non-streaming
			resp := openai.ChatCompletionResponse{
				ID:    "chatcmpl-test",
				Model: "gpt-4o",
				Choices: []openai.Choice{
					{
						Index: 0,
						Message: openai.ChatMessage{
							Role:    "assistant",
							Content: "Hello from mock Copilot!",
						},
						FinishReason: "stop",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"id":   "gpt-4o",
						"name": "GPT-4o",
						"capabilities": map[string]any{
							"type": "chat",
						},
						"policy": map[string]any{
							"state": "enabled",
						},
					},
					{
						"id":   "disabled-model",
						"name": "Disabled Model",
						"capabilities": map[string]any{
							"type": "chat",
						},
						"policy": map[string]any{
							"state": "disabled",
						},
					},
					{
						"id":   "text-embedding-3",
						"name": "Embedding",
						"capabilities": map[string]any{
							"type": "embeddings",
						},
					},
				},
			})

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient("copilot", "tid=test-session-token", server.URL, nil, server.Client())

	ctx := context.Background()

	// 1. Test Execute
	req := &canonical.CanonicalRequest{
		Model: "gpt-4o",
		Messages: []canonical.Message{
			{
				Role:  canonical.RoleUser,
				Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "Hi"}},
			},
		},
	}

	resp, err := client.Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(resp.Message.Parts) == 0 || resp.Message.Parts[0].Text != "Hello from mock Copilot!" {
		t.Fatalf("unexpected execute response: %+v", resp)
	}

	// 2. Test Stream
	eventCh, err := client.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}

	var textReceived string
	for ev := range eventCh {
		if ev.Type == canonical.EventTextDelta {
			textReceived += ev.Text
		}
	}
	if textReceived != "Hello World" {
		t.Fatalf("expected 'Hello World', got %q", textReceived)
	}

	// 3. Test ListModels
	models, err := client.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-4o" {
		t.Fatalf("expected only enabled gpt-4o, got: %+v", models)
	}
}

func TestResolveGitHubToken(t *testing.T) {
	// Explicit token takes priority
	token := ResolveGitHubToken("explicit-token")
	if token != "explicit-token" {
		t.Fatalf("expected explicit-token, got %s", token)
	}

	// Environment variable fallback
	t.Setenv("COPILOT_API_KEY", "env-copilot-token")
	token = ResolveGitHubToken("")
	if token != "env-copilot-token" {
		t.Fatalf("expected env-copilot-token, got %s", token)
	}

	// Windows APPDATA GitHub CLI hosts.yml discovery
	t.Run("Windows APPDATA GitHub CLI", func(t *testing.T) {
		tempDir := t.TempDir()
		ghDir := filepath.Join(tempDir, "GitHub CLI")
		if err := os.MkdirAll(ghDir, 0700); err != nil {
			t.Fatal(err)
		}
		hostsYAML := "github.com:\n    oauth_token: gho_windows_appdata_test_12345\n    user: testuser\n"
		if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(hostsYAML), 0600); err != nil {
			t.Fatal(err)
		}

		t.Setenv("HOME", tempDir)
		t.Setenv("USERPROFILE", tempDir)
		t.Setenv("APPDATA", tempDir)
		t.Setenv("COPILOT_API_KEY", "")
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GH_CONFIG_DIR", "")

		tok := ResolveGitHubToken("")
		if tok != "gho_windows_appdata_test_12345" {
			t.Fatalf("expected token from Windows APPDATA GitHub CLI, got: %s", tok)
		}
	})

	// Windows LOCALAPPDATA Copilot hosts.json discovery
	t.Run("Windows LOCALAPPDATA Copilot", func(t *testing.T) {
		tempDir := t.TempDir()
		copilotDir := filepath.Join(tempDir, "github-copilot")
		if err := os.MkdirAll(copilotDir, 0700); err != nil {
			t.Fatal(err)
		}
		hostsJSON := `{"github.com": {"oauth_token": "ghu_windows_localappdata_copilot_987", "user": "winuser"}}`
		if err := os.WriteFile(filepath.Join(copilotDir, "hosts.json"), []byte(hostsJSON), 0600); err != nil {
			t.Fatal(err)
		}

		t.Setenv("HOME", tempDir)
		t.Setenv("USERPROFILE", tempDir)
		t.Setenv("LOCALAPPDATA", tempDir)
		t.Setenv("APPDATA", "")
		t.Setenv("COPILOT_API_KEY", "")
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")

		tok := ResolveGitHubToken("")
		if tok != "ghu_windows_localappdata_copilot_987" {
			t.Fatalf("expected token from Windows LOCALAPPDATA Copilot, got: %s", tok)
		}
	})
}
