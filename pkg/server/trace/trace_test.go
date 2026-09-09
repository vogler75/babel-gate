package trace

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRequestTrace(t *testing.T) {
	tr := New("POST", "/v1/messages")
	tr.SetRoute("claude-3-7-sonnet", "copilot", "https://api.githubcopilot.com", "claude-3-7-sonnet")
	tr.SetReadDuration(15 * time.Millisecond)

	time.Sleep(10 * time.Millisecond)
	tr.MarkFirstToken()

	if !tr.HasFirstToken() {
		t.Fatalf("expected HasFirstToken() to be true")
	}

	time.Sleep(20 * time.Millisecond)
	tr.SetTokens(1250, 450)
	tr.MarkStreamDone()

	if !tr.IsDone() {
		t.Fatalf("expected IsDone() to be true")
	}

	routeStr := tr.FormatRoute()
	if !strings.Contains(routeStr, "claude-3-7-sonnet via copilot -> https://api.githubcopilot.com") {
		t.Errorf("expected route string to contain provider and destination, got: %s", routeStr)
	}
	if !strings.Contains(routeStr, "1,250 in / 450 out") {
		t.Errorf("expected route string to contain token count, got: %s", routeStr)
	}

	breakdown := tr.FormatBreakdown()
	if !strings.Contains(breakdown, "read: 15ms") {
		t.Errorf("expected breakdown to contain read duration, got: %s", breakdown)
	}
	if !strings.Contains(breakdown, "ttft:") {
		t.Errorf("expected breakdown to contain ttft, got: %s", breakdown)
	}
	if !strings.Contains(breakdown, "stream:") {
		t.Errorf("expected breakdown to contain stream duration, got: %s", breakdown)
	}
	if !strings.Contains(breakdown, "tok/s") {
		t.Errorf("expected breakdown to contain tokens per second, got: %s", breakdown)
	}
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	if tr := FromContext(ctx); tr != nil {
		t.Errorf("expected nil trace from empty context, got: %v", tr)
	}

	tr := New("GET", "/test")
	ctx = WithTrace(ctx, tr)
	if retrieved := FromContext(ctx); retrieved != tr {
		t.Errorf("expected %v, got %v", tr, retrieved)
	}
}

func TestNonStreamingTrace(t *testing.T) {
	tr := New("POST", "/v1/chat/completions")
	tr.SetRoute("gpt-4o", "openai", "https://api.openai.com/v1", "gpt-4o")
	tr.SetReadDuration(5 * time.Millisecond)
	tr.SetUpstreamDuration(350 * time.Millisecond)
	tr.SetTokens(100, 50)

	routeStr := tr.FormatRoute()
	if !strings.Contains(routeStr, "gpt-4o via openai -> https://api.openai.com/v1") {
		t.Errorf("expected route string to contain destination, got: %s", routeStr)
	}

	breakdown := tr.FormatBreakdown()
	if !strings.Contains(breakdown, "upstream: 350ms") {
		t.Errorf("expected breakdown to contain upstream duration, got: %s", breakdown)
	}
}
