package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_BasicOperations(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "metrics_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metrics.db")
	store, err := NewStore(dbPath, 90)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer store.Close()

	// Base time: 2026-09-08 10:15:30 UTC
	baseTime := time.Date(2026, 9, 8, 10, 15, 30, 0, time.UTC)

	// Record request 1 in hour 10
	err = store.Record(baseTime, "anthropic", "claude-3-7-sonnet", 100, 50, 150, false)
	if err != nil {
		t.Fatalf("failed to record metric: %v", err)
	}

	// Record request 2 in hour 10 for same model (should increment counters in same bucket)
	err = store.Record(baseTime.Add(10*time.Minute), "anthropic", "claude-3-7-sonnet", 200, 100, 300, false)
	if err != nil {
		t.Fatalf("failed to record metric: %v", err)
	}

	// Record request 3 in hour 10 for different model
	err = store.Record(baseTime.Add(20*time.Minute), "google", "gemini-2.5-pro", 50, 25, 75, false)
	if err != nil {
		t.Fatalf("failed to record metric: %v", err)
	}

	// Record error in hour 11
	err = store.Record(baseTime.Add(1*time.Hour), "openai", "gpt-4o", 10, 0, 10, true)
	if err != nil {
		t.Fatalf("failed to record metric: %v", err)
	}

	// Query Hourly Metrics for 2026-09-08
	hourly, err := store.GetHourlyMetrics(baseTime, "")
	if err != nil {
		t.Fatalf("failed to get hourly metrics: %v", err)
	}
	if len(hourly) != 24 {
		t.Fatalf("expected 24 hourly buckets, got %d", len(hourly))
	}

	// Hour 10 should have 2 models
	h10 := hourly[10]
	if h10.Requests != 3 {
		t.Errorf("hour 10 requests expected 3, got %d", h10.Requests)
	}
	if h10.TotalTokens != 525 {
		t.Errorf("hour 10 total tokens expected 525 (150+300+75), got %d", h10.TotalTokens)
	}
	if len(h10.Models) != 2 {
		t.Fatalf("hour 10 expected 2 models, got %d", len(h10.Models))
	}
	// Verify models are sorted by TotalTokens descending
	if h10.Models[0].Model != "claude-3-7-sonnet" || h10.Models[0].TotalTokens != 450 {
		t.Errorf("expected top model to be claude-3-7-sonnet with 450 tokens, got %s with %d", h10.Models[0].Model, h10.Models[0].TotalTokens)
	}
	if h10.Models[1].Model != "gemini-2.5-pro" || h10.Models[1].TotalTokens != 75 {
		t.Errorf("expected second model to be gemini-2.5-pro with 75 tokens, got %s with %d", h10.Models[1].Model, h10.Models[1].TotalTokens)
	}

	// Hour 11 should have 1 error
	h11 := hourly[11]
	if h11.Errors != 1 {
		t.Errorf("hour 11 errors expected 1, got %d", h11.Errors)
	}

	// Query Daily Metrics for 2026-09-08 to 2026-09-09
	daily, err := store.GetDailyMetrics(baseTime, baseTime.Add(24*time.Hour), "")
	if err != nil {
		t.Fatalf("failed to get daily metrics: %v", err)
	}
	if len(daily) < 2 {
		t.Fatalf("expected at least 2 days, got %d", len(daily))
	}
	d0 := daily[0]
	if d0.Date != "2026-09-08" {
		t.Errorf("expected day 0 date 2026-09-08, got %s", d0.Date)
	}
	if d0.TotalTokens != 535 {
		t.Errorf("expected total tokens 535, got %d", d0.TotalTokens)
	}
	if d0.Requests != 4 {
		t.Errorf("expected requests 4, got %d", d0.Requests)
	}
	if len(d0.Models) != 3 {
		t.Errorf("expected 3 models, got %d", len(d0.Models))
	}

	// Test Provider Filter
	anthDaily, err := store.GetDailyMetrics(baseTime, baseTime, "anthropic")
	if err != nil {
		t.Fatalf("failed to get filtered daily metrics: %v", err)
	}
	if len(anthDaily) != 1 {
		t.Fatalf("expected 1 day, got %d", len(anthDaily))
	}
	if anthDaily[0].TotalTokens != 450 {
		t.Errorf("expected 450 tokens for anthropic, got %d", anthDaily[0].TotalTokens)
	}

	// Test Summary
	summary, err := store.GetSummary(baseTime, baseTime.Add(24*time.Hour), "")
	if err != nil {
		t.Fatalf("failed to get summary: %v", err)
	}
	if summary.TotalTokens != 535 {
		t.Errorf("expected summary total tokens 535, got %d", summary.TotalTokens)
	}
	if summary.Requests != 4 {
		t.Errorf("expected summary requests 4, got %d", summary.Requests)
	}
	if len(summary.AvailableProviders) != 3 {
		t.Errorf("expected 3 available providers, got %d", len(summary.AvailableProviders))
	}
	if len(summary.AvailableModels) != 3 {
		t.Errorf("expected 3 available models, got %d", len(summary.AvailableModels))
	}
}

func TestStore_PurgeOldMetrics(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "metrics_purge_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metrics.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	oldTime := now.AddDate(0, 0, -40)

	// Record an old entry
	_ = store.Record(oldTime, "openai", "gpt-4o", 100, 50, 150, false)
	// Record a current entry
	_ = store.Record(now, "openai", "gpt-4o", 100, 50, 150, false)

	// Purge older than 30 days
	deleted, err := store.PurgeOldMetrics(30)
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 record purged, got %d", deleted)
	}

	summary, err := store.GetSummary(oldTime.AddDate(0, 0, -5), now, "")
	if err != nil {
		t.Fatalf("get summary failed: %v", err)
	}
	if summary.Requests != 1 {
		t.Errorf("expected 1 remaining request, got %d", summary.Requests)
	}
}
