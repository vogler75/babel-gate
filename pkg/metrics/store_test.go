package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/session"
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

func TestStore_SessionPersistence(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "metrics_sess_test_*")
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

	now := time.Now().UTC()
	sess := &session.Session{
		ID:                     "sess_test_123",
		Client:                 "Claude Code",
		LastProtocol:           "anthropic",
		ClientIP:               "127.0.0.1",
		UserAgent:              "claude-code/1.0",
		CreatedAt:              now.Add(-10 * time.Minute),
		LastActive:             now,
		LastModel:              "openai/gpt-4o",
		RequestCount:           2,
		ContextTokens:          150,
		ContextTokensEstimated: false,
		InputTokens:            300,
		OutputTokens:           100,
		TotalTokens:            400,
		TokensPerSecond:        25.5,
		GenerationDurationMs:   4000,
		MeasuredOutputTokens:   100,
		Models:                 []string{"google/gemini-2.5-pro", "openai/gpt-4o"},
		ModelStats: map[string]*session.ModelUsage{
			"google/gemini-2.5-pro": {Model: "google/gemini-2.5-pro", RequestCount: 1, InputTokens: 150, OutputTokens: 50, TotalTokens: 200, PercentReq: 50, PercentTok: 50},
			"openai/gpt-4o":         {Model: "openai/gpt-4o", RequestCount: 1, InputTokens: 150, OutputTokens: 50, TotalTokens: 200, PercentReq: 50, PercentTok: 50},
		},
	}

	if err := store.SaveSession(sess); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	req1 := session.RequestRecord{
		ID:                   "req_1",
		Timestamp:            now.Add(-5 * time.Minute),
		Protocol:             "anthropic",
		Provider:             "google",
		Model:                "google/gemini-2.5-pro",
		Stream:               true,
		DurationMs:           2500,
		GenerationDurationMs: 2000,
		InputTokens:          150,
		OutputTokens:         50,
		TotalTokens:          200,
		TokensPerSecond:      25.0,
		Status:               "success",
	}
	req2 := session.RequestRecord{
		ID:                   "req_2",
		Timestamp:            now,
		Protocol:             "openai",
		Provider:             "openai",
		Model:                "openai/gpt-4o",
		Stream:               false,
		DurationMs:           2000,
		GenerationDurationMs: 2000,
		InputTokens:          150,
		OutputTokens:         50,
		TotalTokens:          200,
		TokensPerSecond:      25.0,
		Status:               "success",
	}

	if err := store.SaveRequest(sess.ID, req1); err != nil {
		t.Fatalf("SaveRequest req1 failed: %v", err)
	}
	if err := store.SaveRequest(sess.ID, req2); err != nil {
		t.Fatalf("SaveRequest req2 failed: %v", err)
	}

	// Load active sessions
	loaded, err := store.LoadActiveSessions(now.Add(-1*time.Hour), 10, 10)
	if err != nil {
		t.Fatalf("LoadActiveSessions failed: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 session loaded, got %d", len(loaded))
	}

	loadedSess := loaded[0]
	if loadedSess.ID != sess.ID {
		t.Errorf("expected session ID %s, got %s", sess.ID, loadedSess.ID)
	}
	if loadedSess.Client != sess.Client {
		t.Errorf("expected client %s, got %s", sess.Client, loadedSess.Client)
	}
	if loadedSess.LastProtocol != "anthropic" {
		t.Errorf("expected last protocol anthropic, got %s", loadedSess.LastProtocol)
	}
	if loadedSess.TotalTokens != 400 {
		t.Errorf("expected 400 total tokens, got %d", loadedSess.TotalTokens)
	}
	if loadedSess.LastModel != "openai/gpt-4o" {
		t.Errorf("expected LastModel openai/gpt-4o, got %s", loadedSess.LastModel)
	}
	if len(loadedSess.ModelStats) != 2 {
		t.Fatalf("expected 2 ModelStats, got %d", len(loadedSess.ModelStats))
	}
	if loadedSess.ModelStats["openai/gpt-4o"].RequestCount != 1 {
		t.Errorf("expected gpt-4o request count 1, got %d", loadedSess.ModelStats["openai/gpt-4o"].RequestCount)
	}
	if len(loadedSess.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(loadedSess.Models))
	}
	if len(loadedSess.RecentRequests) != 2 {
		t.Fatalf("expected 2 recent requests, got %d", len(loadedSess.RecentRequests))
	}
	// Recent requests must be ordered newest first (req2 then req1)
	if loadedSess.RecentRequests[0].ID != "req_2" {
		t.Errorf("expected most recent request to be req_2, got %s", loadedSess.RecentRequests[0].ID)
	}
	if loadedSess.RecentRequests[0].Protocol != "openai" || loadedSess.RecentRequests[1].Protocol != "anthropic" {
		t.Errorf("request protocols were not restored: %+v", loadedSess.RecentRequests)
	}
	if loadedSess.RecentRequests[1].ID != "req_1" {
		t.Errorf("expected second request to be req_1, got %s", loadedSess.RecentRequests[1].ID)
	}

	// Delete session
	if err := store.DeleteSession(sess.ID); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}

	loadedAfterDelete, err := store.LoadActiveSessions(now.Add(-1*time.Hour), 10, 10)
	if err != nil {
		t.Fatalf("LoadActiveSessions after delete failed: %v", err)
	}
	if len(loadedAfterDelete) != 0 {
		t.Errorf("expected 0 sessions after delete, got %d", len(loadedAfterDelete))
	}
}

func TestStore_GetModelSpeedMetrics(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "speed_metrics_test_*")
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

	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	sessID := "sess_speed_test"

	sess := &session.Session{
		ID:         sessID,
		Client:     "test",
		CreatedAt:  now,
		LastActive: now,
	}
	if err := store.SaveSession(sess); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Model 1 (Fast model): 400 tokens in 1000ms = 400 tok/s at 12:05
	req1 := session.RequestRecord{
		ID:                   "r1",
		Timestamp:            now.Add(5 * time.Minute),
		Provider:             "google",
		Model:                "google/gemini-flash",
		OutputTokens:         400,
		GenerationDurationMs: 1000,
		TokensPerSecond:      400.0,
		Status:               "success",
	}
	// Model 2 (Slower model): 100 tokens in 1000ms = 100 tok/s at 12:05
	req2 := session.RequestRecord{
		ID:                   "r2",
		Timestamp:            now.Add(5 * time.Minute),
		Provider:             "anthropic",
		Model:                "anthropic/claude-opus",
		OutputTokens:         100,
		GenerationDurationMs: 1000,
		TokensPerSecond:      100.0,
		Status:               "success",
	}
	// Model 2 (Slower model): 300 tokens in 1000ms = 300 tok/s at 12:10
	// Model 2 weighted avg = (100 + 300)*1000 / (1000 + 1000) = 400/2 = 200 tok/s
	req3 := session.RequestRecord{
		ID:                   "r3",
		Timestamp:            now.Add(10 * time.Minute),
		Provider:             "anthropic",
		Model:                "anthropic/claude-opus",
		OutputTokens:         300,
		GenerationDurationMs: 1000,
		TokensPerSecond:      300.0,
		Status:               "success",
	}
	// Anomaly 1: Error status -> should be ignored
	reqErr := session.RequestRecord{
		ID:                   "r_err",
		Timestamp:            now.Add(6 * time.Minute),
		Provider:             "google",
		Model:                "google/gemini-flash",
		OutputTokens:         500,
		GenerationDurationMs: 500,
		TokensPerSecond:      1000.0,
		Status:               "error",
	}
	// Anomaly 2: GenerationDurationMs < 50ms -> should be ignored
	reqMicro := session.RequestRecord{
		ID:                   "r_micro",
		Timestamp:            now.Add(7 * time.Minute),
		Provider:             "google",
		Model:                "google/gemini-flash",
		OutputTokens:         200,
		GenerationDurationMs: 2,
		TokensPerSecond:      100000.0,
		Status:               "success",
	}
	// Anomaly 3: OutputTokens <= 0 -> should be ignored
	reqZero := session.RequestRecord{
		ID:                   "r_zero",
		Timestamp:            now.Add(8 * time.Minute),
		Provider:             "google",
		Model:                "google/gemini-flash",
		OutputTokens:         0,
		GenerationDurationMs: 500,
		TokensPerSecond:      0.0,
		Status:               "success",
	}
	// Anomaly 4: Duration clears the minimum, but implied speed is implausible -> should be ignored
	reqSpike := session.RequestRecord{
		ID:                   "r_spike",
		Timestamp:            now.Add(9 * time.Minute),
		Provider:             "google",
		Model:                "google/gemini-flash",
		OutputTokens:         8_983,
		GenerationDurationMs: 243,
		TokensPerSecond:      36_967.1,
		Status:               "success",
	}

	for _, r := range []session.RequestRecord{req1, req2, req3, reqErr, reqMicro, reqZero, reqSpike} {
		if err := store.SaveRequest(sessID, r); err != nil {
			t.Fatalf("SaveRequest failed: %v", err)
		}
	}

	// 1. Test Hour Granularity
	resp, err := store.GetModelSpeedMetrics(now, now.Add(1*time.Hour), "hour", "")
	if err != nil {
		t.Fatalf("GetModelSpeedMetrics failed: %v", err)
	}

	if len(resp.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(resp.Models))
	}
	// Gemini flash (400 tok/s) should be first, Claude opus (200 tok/s) second
	if resp.Models[0].Model != "google/gemini-flash" || resp.Models[0].AvgTokensPerSecond != 400.0 {
		t.Errorf("expected top model google/gemini-flash at 400 tok/s, got %s at %f", resp.Models[0].Model, resp.Models[0].AvgTokensPerSecond)
	}
	if resp.Models[1].Model != "anthropic/claude-opus" || resp.Models[1].AvgTokensPerSecond != 200.0 {
		t.Errorf("expected second model anthropic/claude-opus at 200 tok/s, got %s at %f", resp.Models[1].Model, resp.Models[1].AvgTokensPerSecond)
	}

	if len(resp.Buckets) != 1 {
		t.Fatalf("expected 1 hour bucket, got %d", len(resp.Buckets))
	}

	// 2. Test Minute Granularity
	respMin, err := store.GetModelSpeedMetrics(now, now.Add(1*time.Hour), "minute", "")
	if err != nil {
		t.Fatalf("GetModelSpeedMetrics minute failed: %v", err)
	}
	if len(respMin.Buckets) != 2 {
		t.Fatalf("expected 2 minute buckets (12:05 and 12:10), got %d", len(respMin.Buckets))
	}

	// 3. Test Provider Filter
	respFilter, err := store.GetModelSpeedMetrics(now, now.Add(1*time.Hour), "hour", "google")
	if err != nil {
		t.Fatalf("GetModelSpeedMetrics filtered failed: %v", err)
	}
	if len(respFilter.Models) != 1 || respFilter.Models[0].Provider != "google" {
		t.Fatalf("expected 1 google model, got %+v", respFilter.Models)
	}
}

func TestIssue2DetailedGoogleUsagePersistence(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "detailed.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.RecordDetailed(now, "google", "google/gemini-test", 100, 40, 140, 60, 25, false); err != nil {
		t.Fatal(err)
	}
	var cached, reasoning int
	if err := store.db.QueryRow(`SELECT cached_input_tokens, reasoning_tokens FROM hourly_metrics WHERE provider = 'google' AND model = 'google/gemini-test'`).Scan(&cached, &reasoning); err != nil {
		t.Fatal(err)
	}
	if cached != 60 || reasoning != 25 {
		t.Fatalf("detailed hourly usage lost: cached=%d reasoning=%d", cached, reasoning)
	}
	summary, err := store.GetSummary(now, now, "google")
	if err != nil {
		t.Fatal(err)
	}
	if summary.CachedInputTokens != 60 || summary.ReasoningTokens != 25 || len(summary.TopModels) != 1 || summary.TopModels[0].ReasoningTokens != 25 {
		t.Fatalf("detailed usage is not exposed by metrics queries: %+v", summary)
	}

	sess := &session.Session{ID: "detail", Client: "test", CreatedAt: now, LastActive: now, Models: []string{}, ModelStats: map[string]*session.ModelUsage{}}
	if err := store.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	rec := session.RequestRecord{ID: "request-detail", Timestamp: now, Provider: "google", Model: "google/gemini-test", InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CachedInputTokens: 60, ReasoningTokens: 25, Status: "success"}
	if err := store.SaveRequest(sess.ID, rec); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadActiveSessions(now.Add(-time.Minute), 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || len(loaded[0].RecentRequests) != 1 || loaded[0].RecentRequests[0].CachedInputTokens != 60 || loaded[0].RecentRequests[0].ReasoningTokens != 25 {
		t.Fatalf("detailed request usage lost after reload: %+v", loaded)
	}
}
