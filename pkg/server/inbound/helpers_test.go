package inbound

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vogler75/babel-gate/pkg/session"
)

func TestProtocolFromRequest(t *testing.T) {
	tests := []struct {
		path     string
		headers  map[string]string
		expected string
	}{
		{"/v1/messages", nil, ProtocolAnthropic},
		{"/claude/v1/messages", nil, ProtocolAnthropic},
		{"/anthropic/v1/models", nil, ProtocolAnthropic},
		{"/v1/chat/completions", nil, ProtocolOpenAI},
		{"/openai/v1/responses", nil, ProtocolOpenAI},
		{"/v1beta/models/gemini:generateContent", nil, ProtocolGoogle},
		{"/google/v1/models", nil, ProtocolGoogle},
		{"/gemini/v1beta/models", nil, ProtocolGoogle},
		{"/v1/models", nil, ProtocolOpenAI},
		{"/v1/models?format=anthropic", nil, ProtocolAnthropic},
		{"/v1/models", map[string]string{"x-goog-api-key": "key"}, ProtocolGoogle},
		{"/v1/models", map[string]string{"x-api-key": "key"}, ProtocolOpenAI},
	}

	for _, tc := range tests {
		t.Run(tc.path+tc.expected, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			if got := ProtocolFromRequest(req); got != tc.expected {
				t.Fatalf("ProtocolFromRequest() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestResolveSessionKeepsClientAndProtocolSeparate(t *testing.T) {
	sessions := session.NewManager()
	handler := WithProtocol(ProtocolGoogle, func(w http.ResponseWriter, r *http.Request) {
		sess := ResolveSession(sessions, r)
		if sess.Client != "Google GenAI Client" {
			t.Errorf("client = %q", sess.Client)
		}
		if sess.LastProtocol != ProtocolGoogle {
			t.Errorf("last protocol = %q", sess.LastProtocol)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/custom", nil)
	req.Header.Set("x-session-id", "protocol-test")
	handler(httptest.NewRecorder(), req)
}
