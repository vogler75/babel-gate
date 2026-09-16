package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/session"
)

type inboundProtocolKey struct{}

const (
	ProtocolAnthropic = "anthropic"
	ProtocolOpenAI    = "openai"
	ProtocolGoogle    = "google"
)

// WithProtocol tags requests handled by a protocol-specific endpoint. Route
// registration is authoritative; path/header detection is only a fallback for
// shared legacy endpoints and middleware.
func WithProtocol(protocol string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), inboundProtocolKey{}, protocol)
		next(w, r.WithContext(ctx))
	}
}

// ProtocolFromRequest returns the inbound wire protocol used by the client.
func ProtocolFromRequest(r *http.Request) string {
	if protocol, ok := r.Context().Value(inboundProtocolKey{}).(string); ok {
		return protocol
	}

	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/anthropic/"), strings.HasPrefix(path, "/claude/"),
		path == "/v1/messages", strings.HasPrefix(path, "/v1/messages/"):
		return ProtocolAnthropic
	case strings.HasPrefix(path, "/openai/"), path == "/v1/chat/completions", path == "/v1/responses":
		return ProtocolOpenAI
	case strings.HasPrefix(path, "/google/"), strings.HasPrefix(path, "/gemini/"),
		path == "/v1beta/models", strings.HasPrefix(path, "/v1beta/models/"), strings.HasPrefix(path, "/v1/models/"):
		return ProtocolGoogle
	case path == "/v1/models":
		format := strings.ToLower(r.URL.Query().Get("format"))
		if format == ProtocolAnthropic || r.Header.Get("anthropic-version") != "" {
			return ProtocolAnthropic
		}
		if format == ProtocolGoogle || format == "gemini" || r.Header.Get("x-goog-api-key") != "" {
			return ProtocolGoogle
		}
		return ProtocolOpenAI
	default:
		return ""
	}
}

func executedTrackingModel(engine *router.Engine, tr *trace.RequestTrace, requestedModel string) (string, string) {
	if tr != nil {
		provider, _, targetModel := tr.RouteInfo()
		if provider != "" && provider != "unknown" && targetModel != "" {
			return provider, provider + "/" + strings.TrimPrefix(targetModel, provider+"/")
		}
	}
	return engine.ResolveTrackingModel(requestedModel)
}

type sseWriter struct {
	w          http.ResponseWriter
	controller *http.ResponseController
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, controller: http.NewResponseController(w)}
}

func (s *sseWriter) flush() error {
	return s.controller.Flush()
}

func (s *sseWriter) event(eventType string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal SSE event %s: %w", eventType, err)
	}
	return s.write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, b)))
}

func (s *sseWriter) data(data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal SSE data: %w", err)
	}
	return s.write(append(append([]byte("data: "), b...), '\n', '\n'))
}

func (s *sseWriter) rawData(data string) error {
	return s.write([]byte("data: " + data + "\n\n"))
}

func (s *sseWriter) write(payload []byte) error {
	n, err := s.w.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return io.ErrShortWrite
	}
	return s.flush()
}

// ExtractSessionID retrieves a session identifier from request headers or query params.
func ExtractSessionID(r *http.Request) string {
	if s := r.Header.Get("x-session-id"); s != "" {
		return s
	}
	if s := r.Header.Get("session-id"); s != "" {
		return s
	}
	if s := r.Header.Get("anthropic-session-id"); s != "" {
		return s
	}
	if s := r.Header.Get("x-conversation-id"); s != "" {
		return s
	}
	if s := r.Header.Get("conversation-id"); s != "" {
		return s
	}
	if s := r.Header.Get("x-claude-code-session-id"); s != "" {
		return s
	}
	if s := r.URL.Query().Get("session_id"); s != "" {
		return s
	}
	return ""
}

func generationDurationMs(tr *trace.RequestTrace, fallback time.Duration) int64 {
	if tr != nil {
		if duration := tr.GenerationDuration(); duration > 0 {
			return duration.Milliseconds()
		}
	}
	return fallback.Milliseconds()
}

// ExtractClientIP retrieves the client IP address from request headers or RemoteAddr.
func ExtractClientIP(r *http.Request) string {
	if fwd := r.Header.Get("x-forwarded-for"); fwd != "" {
		parts := strings.Split(fwd, ",")
		return strings.TrimSpace(parts[0])
	}
	if ip := r.Header.Get("x-real-ip"); ip != "" {
		return ip
	}
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx != -1 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

// ResolveSession gets or creates a session for the incoming HTTP request.
func ResolveSession(sessions *session.Manager, r *http.Request) *session.Session {
	if sessions == nil {
		return nil
	}
	sessID := ExtractSessionID(r)
	clientIP := ExtractClientIP(r)
	clientHeader := r.Header.Get("x-client")
	protocol := ProtocolFromRequest(r)
	clientName := session.DetectClientForProtocol(clientHeader, r.UserAgent(), protocol)
	return sessions.GetOrCreateWithProtocol(sessID, clientIP, r.UserAgent(), clientName, protocol)
}

// StreamUsageTracker maintains input, output, and total token accounting for streaming requests.
type StreamUsageTracker struct {
	InTokens          int
	OutTokens         int
	TotalTokens       int
	CachedInputTokens int
	ReasoningTokens   int
	InputEstimated    bool
}

func NewStreamUsageTracker(estInTokens int) *StreamUsageTracker {
	return &StreamUsageTracker{
		InTokens:       estInTokens,
		InputEstimated: true,
	}
}

func (t *StreamUsageTracker) ApplyDelta(usage *canonical.Usage) {
	if usage == nil {
		return
	}
	if usage.PromptTokens > 0 {
		t.InTokens = usage.PromptTokens
		t.InputEstimated = false
	}
	if usage.CompletionTokens > 0 {
		t.OutTokens = usage.CompletionTokens
	}
	if usage.CacheReadInputTokens > 0 {
		t.CachedInputTokens = usage.CacheReadInputTokens
	}
	if usage.ReasoningTokens > 0 {
		t.ReasoningTokens = usage.ReasoningTokens
	}
	if usage.TotalTokens > 0 {
		t.TotalTokens = usage.TotalTokens
	}
}

func (t *StreamUsageTracker) ResolveTotal() int {
	if t.TotalTokens > 0 {
		return t.TotalTokens
	}
	return t.InTokens + t.OutTokens
}

func (t *StreamUsageTracker) Finalize(fallbackOutTokens int) {
	if t.OutTokens == 0 {
		t.OutTokens = fallbackOutTokens
	}
	if t.TotalTokens == 0 {
		t.TotalTokens = t.InTokens + t.OutTokens
	}
}
