package session

import (
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func TestSessionManagerCreationAndReuse(t *testing.T) {
	mgr := NewManager()

	// 1. Auto-generate session
	s1 := mgr.GetOrCreate("", "127.0.0.1", "claude-code/0.2.29", "Claude Code")
	if s1 == nil || s1.ID == "" {
		t.Fatalf("expected valid session, got nil or empty ID")
	}
	if s1.Client != "Claude Code" {
		t.Errorf("expected client 'Claude Code', got %q", s1.Client)
	}

	// 2. Reuse within idle timeout
	s2 := mgr.GetOrCreate("", "127.0.0.1", "claude-code/0.2.29", "Claude Code")
	if s2.ID != s1.ID {
		t.Errorf("expected session reuse with same ID, got %s vs %s", s2.ID, s1.ID)
	}

	// 3. Different client should create different session
	s3 := mgr.GetOrCreate("", "127.0.0.1", "Mozilla/5.0", "Web Playground")
	if s3.ID == s1.ID {
		t.Errorf("expected different session for different client, got same ID: %s", s3.ID)
	}

	// 4. Explicit session ID
	s4 := mgr.GetOrCreate("custom-session-123", "10.0.0.1", "curl/8.1", "cURL")
	if s4.ID != "custom-session-123" {
		t.Errorf("expected session ID 'custom-session-123', got %s", s4.ID)
	}
}

func TestSessionManagerRecordRequestAndSummary(t *testing.T) {
	mgr := NewManager()
	s := mgr.GetOrCreate("sess-test-1", "127.0.0.1", "test-agent", "TestClient")

	// Record request 1
	mgr.RecordRequest(s.ID, RequestRecord{
		Model:                "google/gemini-2.5-pro",
		Stream:               true,
		DurationMs:           250,
		GenerationDurationMs: 200,
		InputTokens:          120,
		OutputTokens:         80,
		Status:               "success",
	})

	// Record request 2
	mgr.RecordRequest(s.ID, RequestRecord{
		Model:        "openai/gpt-4o",
		Stream:       false,
		DurationMs:   350,
		InputTokens:  50,
		OutputTokens: 30,
		Status:       "success",
	})

	list := mgr.ListSessions()
	if len(list) != 1 {
		t.Fatalf("expected 1 session in list, got %d", len(list))
	}

	sess := list[0]
	if sess.RequestCount != 2 {
		t.Errorf("expected 2 requests, got %d", sess.RequestCount)
	}
	if sess.InputTokens != 170 {
		t.Errorf("expected 170 input tokens, got %d", sess.InputTokens)
	}
	if sess.ContextTokens != 50 {
		t.Errorf("expected latest context size of 50 tokens, got %d", sess.ContextTokens)
	}
	if sess.OutputTokens != 110 {
		t.Errorf("expected 110 output tokens, got %d", sess.OutputTokens)
	}
	if sess.TotalTokens != 280 {
		t.Errorf("expected 280 total tokens, got %d", sess.TotalTokens)
	}
	if len(sess.Models) != 2 {
		t.Errorf("expected 2 models tracked, got %d", len(sess.Models))
	}
	if len(sess.RecentRequests) != 2 {
		t.Errorf("expected 2 recent requests, got %d", len(sess.RecentRequests))
	}
	if got := sess.RecentRequests[1].TokensPerSecond; got != 400 {
		t.Errorf("expected first request speed 400 tok/s, got %.2f", got)
	}
	if got := sess.TokensPerSecond; got < 199.9 || got > 200.1 {
		t.Errorf("expected weighted session speed 200 tok/s, got %.2f", got)
	}

	summary := mgr.GetSummary()
	if summary.TotalSessions != 1 {
		t.Errorf("expected 1 total session in summary, got %d", summary.TotalSessions)
	}
	if summary.TotalRequests != 2 {
		t.Errorf("expected 2 total requests in summary, got %d", summary.TotalRequests)
	}
	if summary.TotalInputTokens != 170 {
		t.Errorf("expected 170 total input tokens, got %d", summary.TotalInputTokens)
	}
	if summary.TotalOutputTokens != 110 {
		t.Errorf("expected 110 total output tokens, got %d", summary.TotalOutputTokens)
	}
	if summary.TotalTokens != 280 {
		t.Errorf("expected 280 total tokens, got %d", summary.TotalTokens)
	}

	// Test Clear
	mgr.Clear()
	if len(mgr.ListSessions()) != 0 {
		t.Errorf("expected 0 sessions after Clear()")
	}
	cleanSummary := mgr.GetSummary()
	if cleanSummary.TotalTokens != 0 {
		t.Errorf("expected 0 total tokens after Clear()")
	}
}

func TestContextTracksLatestRequestIncludingEstimatesAndCompaction(t *testing.T) {
	mgr := NewManager()
	sess := mgr.GetOrCreate("sess-context", "127.0.0.1", "test-agent", "TestClient")
	for _, rec := range []RequestRecord{
		{InputTokens: 1000000, Status: "success"},
		{InputTokens: 400000, InputTokensEstimated: true, Status: "error"},
		{InputTokens: 20000, Status: "success"},
	} {
		mgr.RecordRequest(sess.ID, rec)
		got := mgr.ListSessions()[0]
		if got.ContextTokens != rec.InputTokens || got.ContextTokensEstimated != rec.InputTokensEstimated {
			t.Fatalf("context measurement lost: %+v", got)
		}
	}
}

func TestEstimateRequestTokens(t *testing.T) {
	req := &canonical.CanonicalRequest{
		Messages: []canonical.Message{
			{
				Role: "user",
				Parts: []canonical.ContentPart{
					{Type: canonical.PartText, Text: "Hello, can you explain quantum computing?"},
				},
			},
		},
	}

	tokens := EstimateRequestTokens(req)
	if tokens < 5 || tokens > 20 {
		t.Errorf("expected estimated tokens around 10, got %d", tokens)
	}
}

func TestDetectClient(t *testing.T) {
	tests := []struct {
		header   string
		ua       string
		expected string
	}{
		{"MyCustomApp", "any", "MyCustomApp"},
		{"", "Claude-Code/0.2.29 darwin-arm64", "Claude Code"},
		{"", "@anthropic-ai/sdk 0.18.0", "Anthropic SDK"},
		{"", "OpenAI/Python 1.12.0", "OpenAI SDK"},
		{"", "curl/7.88.1", "cURL"},
		{"", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", "Web Browser"},
		{"", "SomeOtherAgent/1.0", "API Client"},
	}

	for _, tt := range tests {
		got := DetectClient(tt.header, tt.ua)
		if got != tt.expected {
			t.Errorf("DetectClient(%q, %q) = %q, expected %q", tt.header, tt.ua, got, tt.expected)
		}
	}
}

func TestDeleteSessionAndEviction(t *testing.T) {
	mgr := NewManager()
	mgr.maxSessions = 3 // small cap for testing eviction

	_ = mgr.GetOrCreate("sess-1", "127.0.0.1", "agent-1", "Client1")
	_ = mgr.GetOrCreate("sess-2", "127.0.0.1", "agent-2", "Client2")
	_ = mgr.GetOrCreate("sess-3", "127.0.0.1", "agent-3", "Client3")

	if len(mgr.ListSessions()) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(mgr.ListSessions()))
	}

	// Delete s2
	deleted := mgr.DeleteSession("sess-2")
	if !deleted {
		t.Errorf("expected DeleteSession to return true for existing session")
	}
	if len(mgr.ListSessions()) != 2 {
		t.Errorf("expected 2 sessions after deletion, got %d", len(mgr.ListSessions()))
	}

	// Re-add sess-2 and sess-4 (exceeding maxSessions = 3)
	_ = mgr.GetOrCreate("sess-2", "127.0.0.1", "agent-2", "Client2")
	_ = mgr.GetOrCreate("sess-4", "127.0.0.1", "agent-4", "Client4")

	// Count should be capped at maxSessions
	if len(mgr.ListSessions()) > 3 {
		t.Errorf("expected at most 3 sessions due to eviction, got %d", len(mgr.ListSessions()))
	}
}

type mockRecorder struct {
	records []struct {
		provider    string
		model       string
		inTokens    int
		outTokens   int
		totalTokens int
		isError     bool
	}
}

func (m *mockRecorder) Record(t time.Time, provider, model string, inTokens, outTokens, totalTokens int, isError bool) error {
	m.records = append(m.records, struct {
		provider    string
		model       string
		inTokens    int
		outTokens   int
		totalTokens int
		isError     bool
	}{provider, model, inTokens, outTokens, totalTokens, isError})
	return nil
}

func TestSessionManagerMetricsRecorder(t *testing.T) {
	mgr := NewManager()
	rec := &mockRecorder{}
	mgr.SetMetricsRecorder(rec)

	s := mgr.GetOrCreate("sess-rec-1", "127.0.0.1", "test", "Tester")
	mgr.RecordRequest(s.ID, RequestRecord{
		Provider:     "anthropic",
		Model:        "claude-3-7-sonnet",
		InputTokens:  100,
		OutputTokens: 200,
		TotalTokens:  300,
		Status:       "success",
	})

	if len(rec.records) != 1 {
		t.Fatalf("expected 1 record in recorder, got %d", len(rec.records))
	}
	r := rec.records[0]
	if r.provider != "anthropic" || r.model != "anthropic/claude-3-7-sonnet" || r.totalTokens != 300 || r.isError != false {
		t.Errorf("unexpected record: %+v", r)
	}
	if len(s.Models) != 1 || s.Models[0] != "anthropic/claude-3-7-sonnet" {
		t.Errorf("expected session model 'anthropic/claude-3-7-sonnet', got: %+v", s.Models)
	}
}
