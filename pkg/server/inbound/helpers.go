package inbound

import (
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/session"
)

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
