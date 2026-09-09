package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/metrics"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/session"
)

func setupTestDashboard(t *testing.T) (*DashboardHandler, *metrics.Store, func()) {
	tempDir, err := os.MkdirTemp("", "ui_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tempDir, "metrics.db")
	store, err := metrics.NewStore(dbPath, 90)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("failed to create metrics store: %v", err)
	}

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Routing: config.RoutingConfig{
			Routes: map[string]string{},
		},
	}
	engine, err := router.NewEngine(cfg)
	if err != nil {
		store.Close()
		os.RemoveAll(tempDir)
		t.Fatalf("failed to create engine: %v", err)
	}

	catalog := router.NewCatalog(engine)
	sessions := session.NewManager()
	sessions.SetMetricsRecorder(store)

	handler := NewDashboardHandler(engine, catalog, sessions, store)

	cleanup := func() {
		store.Close()
		os.RemoveAll(tempDir)
	}

	return handler, store, cleanup
}

func TestDashboardHandler_MetricsEndpoints(t *testing.T) {
	handler, store, cleanup := setupTestDashboard(t)
	defer cleanup()

	// Record some sample data for 2026-09-08
	testDate := time.Date(2026, 9, 8, 14, 30, 0, 0, time.UTC)
	_ = store.Record(testDate, "anthropic", "claude-3-7-sonnet", 200, 100, 300, false)
	_ = store.Record(testDate, "openai", "gpt-4o", 50, 50, 100, false)
	_ = store.Record(testDate.Add(2*time.Hour), "google", "gemini-2.5-pro", 80, 40, 120, true)

	// 1. Test /api/metrics/summary
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/summary?start=2026-09-07&end=2026-09-09", nil)
	w := httptest.NewRecorder()
	handler.HandleAPIMetricsSummary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var sum metrics.MetricsSummary
	if err := json.NewDecoder(w.Body).Decode(&sum); err != nil {
		t.Fatalf("failed to decode summary json: %v", err)
	}
	if sum.TotalTokens != 520 {
		t.Errorf("expected 520 total tokens, got %d", sum.TotalTokens)
	}
	if sum.Requests != 3 {
		t.Errorf("expected 3 requests, got %d", sum.Requests)
	}
	if sum.Errors != 1 {
		t.Errorf("expected 1 error, got %d", sum.Errors)
	}
	if len(sum.TopModels) != 3 {
		t.Errorf("expected 3 top models, got %d", len(sum.TopModels))
	}
	if sum.TopModels[0].Model != "claude-3-7-sonnet" {
		t.Errorf("expected top model claude-3-7-sonnet, got %s", sum.TopModels[0].Model)
	}

	// 2. Test /api/metrics/daily
	reqDaily := httptest.NewRequest(http.MethodGet, "/api/metrics/daily?start=2026-09-08&end=2026-09-08", nil)
	wDaily := httptest.NewRecorder()
	handler.HandleAPIMetricsDaily(wDaily, reqDaily)

	if wDaily.Code != http.StatusOK {
		t.Fatalf("expected daily status 200, got %d", wDaily.Code)
	}
	var dailyResp struct {
		Days []metrics.DailyMetricBucket `json:"days"`
	}
	if err := json.NewDecoder(wDaily.Body).Decode(&dailyResp); err != nil {
		t.Fatalf("failed to decode daily json: %v", err)
	}
	if len(dailyResp.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(dailyResp.Days))
	}
	d0 := dailyResp.Days[0]
	if d0.Date != "2026-09-08" || d0.TotalTokens != 520 {
		t.Errorf("unexpected daily bucket: %+v", d0)
	}
	if len(d0.Models) != 3 {
		t.Errorf("expected 3 models in day, got %d", len(d0.Models))
	}

	// 3. Test /api/metrics/hourly (zoomed into day)
	reqHourly := httptest.NewRequest(http.MethodGet, "/api/metrics/hourly?date=2026-09-08", nil)
	wHourly := httptest.NewRecorder()
	handler.HandleAPIMetricsHourly(wHourly, reqHourly)

	if wHourly.Code != http.StatusOK {
		t.Fatalf("expected hourly status 200, got %d", wHourly.Code)
	}
	var hourlyResp struct {
		Date  string                       `json:"date"`
		Hours []metrics.HourlyMetricBucket `json:"hours"`
	}
	if err := json.NewDecoder(wHourly.Body).Decode(&hourlyResp); err != nil {
		t.Fatalf("failed to decode hourly json: %v", err)
	}
	if len(hourlyResp.Hours) != 24 {
		t.Fatalf("expected 24 hours, got %d", len(hourlyResp.Hours))
	}
	// Hour 14:00 should have claude-3-7-sonnet and gpt-4o
	h14 := hourlyResp.Hours[14]
	if h14.TotalTokens != 400 {
		t.Errorf("expected 400 tokens in hour 14, got %d", h14.TotalTokens)
	}
	if len(h14.Models) != 2 {
		t.Errorf("expected 2 models in hour 14, got %d", len(h14.Models))
	}
	// Hour 16:00 should have gemini-2.5-pro with error
	h16 := hourlyResp.Hours[16]
	if h16.TotalTokens != 120 || h16.Errors != 1 {
		t.Errorf("expected 120 tokens and 1 error in hour 16, got %+v", h16)
	}

	// 4. Test Index HTML contains analytics elements
	reqIndex := httptest.NewRequest(http.MethodGet, "/", nil)
	wIndex := httptest.NewRecorder()
	handler.HandleIndex(wIndex, reqIndex)
	if wIndex.Code != http.StatusOK {
		t.Fatalf("expected index 200, got %d", wIndex.Code)
	}
	html := wIndex.Body.String()
	if !contains(html, "Historical Token Analytics") {
		t.Errorf("expected 'Historical Token Analytics' in dashboard HTML")
	}
	if !contains(html, "chartSvgWrapper") {
		t.Errorf("expected 'chartSvgWrapper' in dashboard HTML")
	}
	if !contains(html, "zoomIntoDay") {
		t.Errorf("expected 'zoomIntoDay' function in dashboard HTML")
	}
	if !contains(html, "Output Speed") || !contains(html, "tokens_per_second") {
		t.Errorf("expected per-session token generation speed in dashboard HTML")
	}
	if !contains(html, "Context Tokens") || !contains(html, "context_tokens") {
		t.Errorf("expected latest context token count in dashboard session list")
	}
	if !contains(html, "route-provider") || !contains(html, "populateRouteModelSelect") {
		t.Errorf("expected provider-filtered virtual route model selection in dashboard HTML")
	}
	if !contains(html, "Reload from YAML") || !contains(html, "reloadRouting") {
		t.Errorf("expected online route reload control in dashboard HTML")
	}
	if !contains(html, "selectAllModels") || !contains(html, "Generate OpenCode Config") || !contains(html, "generateOpenCodeConfig") {
		t.Errorf("expected selectable models and OpenCode configuration generator in dashboard HTML")
	}
	if !contains(html, "@ai-sdk/openai-compatible") || !contains(html, "copyOpenCodeConfig") {
		t.Errorf("expected OpenCode provider JSON and copy control in dashboard HTML")
	}
	if !contains(html, "modelProviderFilter") || !contains(html, "modelNameFilter") || !contains(html, "filteredCatalogModels") {
		t.Errorf("expected provider and name filters for the model catalog")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) && len(substr) > 0 && findSubstr(s, substr)))
}

func findSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

type mockProvider struct {
	name   string
	pType  string
	models []providers.ModelInfo
}

func (m *mockProvider) Name() string     { return m.name }
func (m *mockProvider) Type() string     { return m.pType }
func (m *mockProvider) Endpoint() string { return "http://mock" }
func (m *mockProvider) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	return nil, nil
}
func (m *mockProvider) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	return nil, nil
}
func (m *mockProvider) ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	return m.models, nil
}

func TestDashboardHandler_APIModels_MultipleProviders(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"google":  {Type: "google", Priority: 1},
			"copilot": {Type: "copilot", Priority: 2},
		},
	}
	engine, err := router.NewEngine(cfg)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	engine.RegisterProvider(&mockProvider{
		name:  "google",
		pType: "google",
		models: []providers.ModelInfo{
			{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash"},
		},
	})
	engine.RegisterProvider(&mockProvider{
		name:  "copilot",
		pType: "copilot",
		models: []providers.ModelInfo{
			{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash (Copilot)"},
		},
	})

	catalog := router.NewCatalog(engine)
	handler := NewDashboardHandler(engine, catalog, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	w := httptest.NewRecorder()
	handler.HandleAPIModels(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var res struct {
		Models []router.CatalogModel `json:"models"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(res.Models) != 2 {
		t.Fatalf("expected 2 models in /api/models, got %d: %+v", len(res.Models), res.Models)
	}

	hasGoogle := false
	hasCopilot := false
	for _, m := range res.Models {
		if m.ID == "gemini-3.5-flash" {
			if m.Provider == "google" {
				hasGoogle = true
			} else if m.Provider == "copilot" {
				hasCopilot = true
			}
		}
	}

	if !hasGoogle {
		t.Errorf("expected gemini-3.5-flash from google")
	}
	if !hasCopilot {
		t.Errorf("expected gemini-3.5-flash from copilot")
	}
}

func TestDashboardHandler_RoutingAPI(t *testing.T) {
	handler, _, cleanup := setupTestDashboard(t)
	defer cleanup()

	body := []byte(`{"default":"","routes":{"fast":"google/gemini-2.5-flash"},"fallbacks":{"fast":["openai/gpt-4o"]}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/routing", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleAPIRouting(w, req)
	// The setup has no providers, so provider-prefixed targets are rejected.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid provider status 400, got %d", w.Code)
	}

	valid := []byte(`{"default":"","routes":{"fast":"virtual-model"},"fallbacks":{}}`)
	req = httptest.NewRequest(http.MethodPut, "/api/routing", bytes.NewReader(valid))
	w = httptest.NewRecorder()
	handler.HandleAPIRouting(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := handler.engine.GetRoutes()["fast"]; got != "virtual-model" {
		t.Fatalf("expected live route update, got %q", got)
	}
}

func TestDashboardHandler_ReloadRoutingAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := "providers:\n  openai:\n    type: openai\nrouting:\n  routes:\n    fast: openai/gpt-4o-mini\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := router.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewDashboardHandler(engine, router.NewCatalog(engine), nil, nil)
	updated := "providers:\n  openai:\n    type: openai\nrouting:\n  routes:\n    fast: openai/gpt-4.1\n"
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/routing", nil)
	w := httptest.NewRecorder()
	handler.HandleAPIRouting(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := engine.GetRoutes()["fast"]; got != "openai/gpt-4.1" {
		t.Fatalf("expected reloaded route, got %q", got)
	}
}

func TestDashboardHandler_ProviderTogglePersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("providers:\n  openai:\n    type: openai\n    enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := router.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewDashboardHandler(engine, router.NewCatalog(engine), nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/providers/openai", bytes.NewBufferString(`{"enabled":true}`))
	w := httptest.NewRecorder()
	handler.HandleAPIProvider(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(engine.GetProviders()) != 1 {
		t.Fatal("provider was not enabled in the running engine")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), "enabled: true") {
		t.Fatalf("provider state was not persisted:\n%s", data)
	}
}
