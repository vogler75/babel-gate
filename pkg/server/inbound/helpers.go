package inbound

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/session"
)

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
	clientName := session.DetectClient(clientHeader, r.UserAgent())
	return sessions.GetOrCreate(sessID, clientIP, r.UserAgent(), clientName)
}
