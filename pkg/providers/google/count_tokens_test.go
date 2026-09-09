package google

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers"
)

func TestCountTokensUsesTranslatedGenerateRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:countTokens" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "secret" {
			t.Errorf("API key was not forwarded")
		}
		var body struct {
			GenerateContentRequest GenerateContentRequest `json:"generateContentRequest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		request := body.GenerateContentRequest
		if request.Model != "models/gemini-test" {
			t.Errorf("unexpected nested model %q", request.Model)
		}
		if len(request.Contents) != 1 || request.SystemInstruction == nil || len(request.Tools) != 1 {
			t.Errorf("translated request content was lost: %+v", request)
		}
		if request.GenerationConfig != nil {
			t.Errorf("generation config should not be sent to countTokens")
		}
		_ = json.NewEncoder(w).Encode(CountTokensResponse{TotalTokens: 1234})
	}))
	defer srv.Close()

	client := NewClient("google", "secret", srv.URL, nil, srv.Client())
	count, err := client.CountTokens(context.Background(), &canonical.CanonicalRequest{
		Model: "gemini-test",
		Messages: []canonical.Message{
			{Role: canonical.RoleSystem, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "Be concise"}}},
			{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "Hello"}}},
		},
		Tools: []canonical.ToolDeclaration{{Name: "lookup", Description: "Look something up", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1234 {
		t.Fatalf("expected 1234 tokens, got %d", count)
	}
}

func TestStreamPreservesGoogleAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"The input token count exceeds the maximum allowed.","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()

	client := NewClient("google", "secret", srv.URL, nil, srv.Client())
	_, err := client.Stream(context.Background(), &canonical.CanonicalRequest{
		Model:    "gemini-test",
		Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "Hello"}}}},
	})
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected structured API error, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Status != "INVALID_ARGUMENT" || !strings.Contains(apiErr.Message, "token count") {
		t.Fatalf("unexpected structured API error: %+v", apiErr)
	}
}

func TestCountTokensRetriesVertexFormatWithoutLosingPrompt(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if calls == 1 {
			if body["generateContentRequest"] == nil {
				t.Error("expected Developer API format first")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid JSON payload received. Unknown name \"generateContentRequest\": Cannot find field."}}`))
			return
		}
		if body["generateContentRequest"] != nil || body["model"] != nil || body["generationConfig"] != nil {
			t.Errorf("unexpected fields in Vertex request: %v", body)
		}
		if body["contents"] == nil || body["systemInstruction"] == nil || body["tools"] == nil {
			t.Errorf("count retry dropped part of the prompt: %v", body)
		}
		_, _ = w.Write([]byte(`{"totalTokens":107500}`))
	}))
	defer srv.Close()
	client := NewClient("google", "secret", srv.URL, nil, srv.Client())
	count, err := client.CountTokens(context.Background(), &canonical.CanonicalRequest{
		Model: "gemini-test",
		Messages: []canonical.Message{
			{Role: canonical.RoleSystem, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "instructions"}}},
			{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "hello"}}},
		},
		Tools: []canonical.ToolDeclaration{{Name: "read", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil || count != 107500 || calls != 2 {
		t.Fatalf("count=%d calls=%d err=%v", count, calls, err)
	}
}

func TestCountTokensRejectsMissingCountAndDoesNotRetryOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing", 200, `{}`},
		{"negative", 200, `{"totalTokens":-1}`},
		{"invalid", 400, `{"error":{"message":"Invalid contents"}}`},
		{"auth", 401, `{"error":{"message":"Unauthorized"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := NewClient("google", "secret", srv.URL, nil, srv.Client()).CountTokens(context.Background(), &canonical.CanonicalRequest{Model: "gemini-test"})
			if err == nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
