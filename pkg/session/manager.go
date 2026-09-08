package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// MetricsRecorder defines an interface for persisting hourly usage counters.
type MetricsRecorder interface {
	Record(t time.Time, provider, model string, inTokens, outTokens, totalTokens int, isError bool) error
}

// RequestRecord captures details of an individual LLM request within a session.
type RequestRecord struct {
	ID           string    `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model"`
	Stream       bool      `json:"stream"`
	DurationMs   int64     `json:"duration_ms"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	Status       string    `json:"status"` // "success" or "error"
	ErrorMessage string    `json:"error_message,omitempty"`
}

// Session tracks an ongoing client conversation/session and its aggregated token usage.
type Session struct {
	ID             string          `json:"id"`
	Client         string          `json:"client"` // e.g. "Claude Code", "Web Playground", "OpenAI SDK"
	ClientIP       string          `json:"client_ip,omitempty"`
	UserAgent      string          `json:"user_agent,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	LastActive     time.Time       `json:"last_active"`
	RequestCount   int             `json:"request_count"`
	InputTokens    int             `json:"input_tokens"`
	OutputTokens   int             `json:"output_tokens"`
	TotalTokens    int             `json:"total_tokens"`
	Models         []string        `json:"models"`
	RecentRequests []RequestRecord `json:"recent_requests,omitempty"`
}

// Summary provides aggregate metrics across all tracked sessions.
type Summary struct {
	TotalSessions     int `json:"total_sessions"`
	TotalRequests     int `json:"total_requests"`
	TotalInputTokens  int `json:"total_input_tokens"`
	TotalOutputTokens int `json:"total_output_tokens"`
	TotalTokens       int `json:"total_tokens"`
}

// Manager manages thread-safe tracking of client sessions and token usage.
type Manager struct {
	mu              sync.RWMutex
	sessions        map[string]*Session
	order           []string // list of session IDs
	idleTimeout     time.Duration
	sessionTTL      time.Duration // auto-purge sessions older than this (e.g. 24h)
	maxSessions     int           // max concurrent sessions to keep in memory (e.g. 200)
	maxRequests     int           // max requests to keep per session
	totalReqs       int
	totalInTok      int
	totalOutTok     int
	metricsRecorder MetricsRecorder
}

// NewManager creates a new Session Manager with retention defaults.
func NewManager() *Manager {
	return &Manager{
		sessions:    make(map[string]*Session),
		order:       make([]string, 0),
		idleTimeout: 30 * time.Minute,
		sessionTTL:  24 * time.Hour,
		maxSessions: 200,
		maxRequests: 50,
	}
}

// SetMetricsRecorder assigns a persistent metrics store to the manager.
func (m *Manager) SetMetricsRecorder(rec MetricsRecorder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metricsRecorder = rec
}

// GenerateID produces a random hexadecimal session/request ID.
func GenerateID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b))
}

// DetectClient inspects headers to identify the client application.
func DetectClient(clientHeader, userAgent string) string {
	if clientHeader != "" {
		return clientHeader
	}
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "claude-code") || strings.Contains(ua, "claudecode"):
		return "Claude Code"
	case strings.Contains(ua, "anthropic"):
		return "Anthropic SDK"
	case strings.Contains(ua, "openai"):
		return "OpenAI SDK"
	case strings.Contains(ua, "curl"):
		return "cURL"
	case strings.Contains(ua, "mozilla") || strings.Contains(ua, "chrome") || strings.Contains(ua, "safari"):
		return "Web Browser"
	case strings.Contains(ua, "postman"):
		return "Postman"
	case strings.Contains(ua, "python"):
		return "Python Client"
	default:
		return "API Client"
	}
}

// GetOrCreate finds an existing active session or creates a new one.
func (m *Manager) GetOrCreate(sessionID, clientIP, userAgent, clientName string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	m.cleanupOldSessionsLocked(now)

	// 1. If explicit sessionID was provided
	if sessionID != "" {
		if s, ok := m.sessions[sessionID]; ok {
			s.LastActive = now
			if clientName != "" && (s.Client == "" || s.Client == "API Client") {
				s.Client = clientName
			}
			return s
		}

		// Create with given ID
		newSess := &Session{
			ID:             sessionID,
			Client:         clientName,
			ClientIP:       clientIP,
			UserAgent:      userAgent,
			CreatedAt:      now,
			LastActive:     now,
			Models:         make([]string, 0),
			RecentRequests: make([]RequestRecord, 0),
		}
		m.sessions[sessionID] = newSess
		m.order = append(m.order, sessionID)
		m.cleanupOldSessionsLocked(now)
		return newSess
	}

	// 2. If no explicit ID, look for recent active session from same clientIP & client
	for i := len(m.order) - 1; i >= 0; i-- {
		id := m.order[i]
		s := m.sessions[id]
		if s != nil && s.ClientIP == clientIP && s.Client == clientName {
			if now.Sub(s.LastActive) <= m.idleTimeout {
				s.LastActive = now
				return s
			}
		}
	}

	// 3. Otherwise, create a new auto-generated session
	newID := GenerateID("sess")
	newSess := &Session{
		ID:             newID,
		Client:         clientName,
		ClientIP:       clientIP,
		UserAgent:      userAgent,
		CreatedAt:      now,
		LastActive:     now,
		Models:         make([]string, 0),
		RecentRequests: make([]RequestRecord, 0),
	}
	m.sessions[newID] = newSess
	m.order = append(m.order, newID)
	m.cleanupOldSessionsLocked(now)
	return newSess
}

// RecordRequest updates a session with a completed request record and aggregates tokens.
func (m *Manager) RecordRequest(sessionID string, rec RequestRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if rec.ID == "" {
		rec.ID = GenerateID("req")
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}
	if rec.TotalTokens == 0 {
		rec.TotalTokens = rec.InputTokens + rec.OutputTokens
	}

	if m.metricsRecorder != nil {
		isErr := rec.Status == "error"
		_ = m.metricsRecorder.Record(rec.Timestamp, rec.Provider, rec.Model, rec.InputTokens, rec.OutputTokens, rec.TotalTokens, isErr)
	}

	s, ok := m.sessions[sessionID]
	if !ok {
		return
	}

	s.LastActive = rec.Timestamp
	s.RequestCount++
	s.InputTokens += rec.InputTokens
	s.OutputTokens += rec.OutputTokens
	s.TotalTokens += rec.TotalTokens

	// Track model if not already present
	if rec.Model != "" {
		hasModel := false
		for _, m := range s.Models {
			if m == rec.Model {
				hasModel = true
				break
			}
		}
		if !hasModel {
			s.Models = append(s.Models, rec.Model)
		}
	}

	// Prepend to recent requests (latest first), capped at maxRequests
	s.RecentRequests = append([]RequestRecord{rec}, s.RecentRequests...)
	if len(s.RecentRequests) > m.maxRequests {
		s.RecentRequests = s.RecentRequests[:m.maxRequests]
	}

	// Global aggregates
	m.totalReqs++
	m.totalInTok += rec.InputTokens
	m.totalOutTok += rec.OutputTokens
}

// ListSessions returns all sessions ordered by LastActive descending.
func (m *Manager) ListSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		// Deep copy recent requests shallowly to prevent race during read
		reqCopy := make([]RequestRecord, len(s.RecentRequests))
		copy(reqCopy, s.RecentRequests)
		modelsCopy := make([]string, len(s.Models))
		copy(modelsCopy, s.Models)

		sessCopy := *s
		sessCopy.RecentRequests = reqCopy
		sessCopy.Models = modelsCopy
		list = append(list, &sessCopy)
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].LastActive.After(list[j].LastActive)
	})

	return list
}

// GetSummary returns total aggregates across all tracked sessions.
func (m *Manager) GetSummary() Summary {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return Summary{
		TotalSessions:     len(m.sessions),
		TotalRequests:     m.totalReqs,
		TotalInputTokens:  m.totalInTok,
		TotalOutputTokens: m.totalOutTok,
		TotalTokens:       m.totalInTok + m.totalOutTok,
	}
}

// DeleteSession removes a single session by its ID.
func (m *Manager) DeleteSession(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.sessions[sessionID]; !ok {
		return false
	}

	delete(m.sessions, sessionID)
	for i, id := range m.order {
		if id == sessionID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return true
}

// Clear removes all sessions and resets cumulative metrics.
func (m *Manager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions = make(map[string]*Session)
	m.order = make([]string, 0)
	m.totalReqs = 0
	m.totalInTok = 0
	m.totalOutTok = 0
}

func (m *Manager) cleanupOldSessionsLocked(now time.Time) {
	// 1. Purge sessions older than sessionTTL
	if m.sessionTTL > 0 {
		cutoff := now.Add(-m.sessionTTL)
		filteredOrder := make([]string, 0, len(m.order))
		for _, id := range m.order {
			s, ok := m.sessions[id]
			if !ok {
				continue
			}
			if s.LastActive.Before(cutoff) {
				delete(m.sessions, id)
			} else {
				filteredOrder = append(filteredOrder, id)
			}
		}
		m.order = filteredOrder
	}

	// 2. Evict oldest sessions if exceeding maxSessions
	if m.maxSessions > 0 && len(m.sessions) > m.maxSessions {
		type sessTime struct {
			id   string
			time time.Time
		}
		stList := make([]sessTime, 0, len(m.sessions))
		for id, s := range m.sessions {
			stList = append(stList, sessTime{id: id, time: s.LastActive})
		}
		sort.Slice(stList, func(i, j int) bool {
			return stList[i].time.Before(stList[j].time)
		})

		excess := len(m.sessions) - m.maxSessions
		for i := 0; i < excess && i < len(stList); i++ {
			delete(m.sessions, stList[i].id)
		}

		newOrder := make([]string, 0, len(m.sessions))
		for _, id := range m.order {
			if _, ok := m.sessions[id]; ok {
				newOrder = append(newOrder, id)
			}
		}
		m.order = newOrder
	}
}

// EstimateTokens provides a fallback token count estimation (~4 characters per token).
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	count := len(text) / 4
	if count < 1 {
		count = 1
	}
	return count
}

// EstimateRequestTokens calculates estimated prompt tokens from a CanonicalRequest.
func EstimateRequestTokens(req *canonical.CanonicalRequest) int {
	if req == nil {
		return 0
	}
	totalChars := 0

	// Messages
	for _, msg := range req.Messages {
		for _, p := range msg.Parts {
			totalChars += len(p.Text)
			totalChars += len(p.Thinking)
			totalChars += len(p.ToolCallArgs)
			totalChars += len(p.ToolResultContent)
		}
	}

	// Tools
	for _, t := range req.Tools {
		totalChars += len(t.Name) + len(t.Description)
		if len(t.Parameters) > 0 {
			if b, err := json.Marshal(t.Parameters); err == nil {
				totalChars += len(b)
			}
		}
	}

	return EstimateTokens(strings.Repeat("a", totalChars))
}
