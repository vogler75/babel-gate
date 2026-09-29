package router

import (
	"context"
	"fmt"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers"
)

// failingProvider returns err from every call and counts attempts.
type failingProvider struct {
	mockProvider
	err   error
	calls int
}

func (f *failingProvider) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.mockProvider.Execute(ctx, req)
}

func (f *failingProvider) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.mockProvider.Stream(ctx, req)
}

func newSmartEngine(t *testing.T, provs ...providers.Provider) *Engine {
	t.Helper()
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Routing:   config.RoutingConfig{Routes: map[string]string{}, Fallbacks: map[string][]string{}},
		Smart: config.SmartConfig{Tiers: map[string][]string{
			"simple":  {"onprem/small"},
			"complex": {"copilot/sonnet", "sdc/sol"},
		}},
	}
	disabled := false
	for _, name := range []string{"onprem", "copilot", "sdc"} {
		cfg.Providers[name] = config.ProviderConfig{Type: "openai", Enabled: &disabled}
	}
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range provs {
		e.RegisterProvider(p)
	}
	return e
}

func complexRequest() *canonical.CanonicalRequest {
	return &canonical.CanonicalRequest{
		Model:    "smart",
		Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "implement a retry wrapper for the HTTP client"}}}},
		Tools:    []canonical.ToolDeclaration{{Name: "Edit"}},
	}
}

func TestSmartFallsBackAndCoolsDownRateLimitedProvider(t *testing.T) {
	copilot := &failingProvider{mockProvider: mockProvider{name: "copilot", pType: "copilot"}, err: fmt.Errorf("copilot api error status 429: quota exceeded")}
	sdc := &failingProvider{mockProvider: mockProvider{name: "sdc", pType: "openai"}}
	onprem := &failingProvider{mockProvider: mockProvider{name: "onprem", pType: "openai"}}
	e := newSmartEngine(t, copilot, sdc, onprem)

	req := complexRequest()
	ctx, decision, err := e.ApplySmart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Tier.String() != "complex" || req.Model != "copilot/sonnet" {
		t.Fatalf("decision = %+v, model = %q", decision, req.Model)
	}
	resp, err := e.Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "sol" || copilot.calls != 1 || sdc.calls != 1 {
		t.Fatalf("resp model %q, copilot calls %d, sdc calls %d", resp.Model, copilot.calls, sdc.calls)
	}

	// The rate-limited provider is now cooling down, so the next request starts at SDC.
	req = complexRequest()
	ctx, _, err = e.ApplySmart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "sdc/sol" {
		t.Fatalf("cooling provider still first: %q", req.Model)
	}
	if _, err := e.Stream(ctx, req); err != nil {
		t.Fatal(err)
	}
	if copilot.calls != 1 {
		t.Fatalf("cooling provider called again: %d", copilot.calls)
	}
}

func TestSmartRequestErrorDoesNotCoolDown(t *testing.T) {
	copilot := &failingProvider{mockProvider: mockProvider{name: "copilot", pType: "copilot"}, err: &providers.APIError{Provider: "copilot", StatusCode: 400, Message: "bad"}}
	sdc := &failingProvider{mockProvider: mockProvider{name: "sdc", pType: "openai"}}
	e := newSmartEngine(t, copilot, sdc)

	req := complexRequest()
	ctx, _, err := e.ApplySmart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(ctx, req); err != nil {
		t.Fatal(err)
	}
	req = complexRequest()
	if _, _, err := e.ApplySmart(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "copilot/sonnet" {
		t.Fatalf("400 should not pause provider, first target = %q", req.Model)
	}
}

func TestSmartSkipsUnavailableProviders(t *testing.T) {
	sdc := &failingProvider{mockProvider: mockProvider{name: "sdc", pType: "openai"}}
	e := newSmartEngine(t, sdc) // copilot and onprem stay disabled

	req := complexRequest()
	if _, _, err := e.ApplySmart(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "sdc/sol" {
		t.Fatalf("model = %q, want sdc/sol", req.Model)
	}

	simple := &canonical.CanonicalRequest{Model: "smart", Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "fix typo"}}}}}
	if _, _, err := e.ApplySmart(context.Background(), simple); err == nil {
		t.Fatal("expected error when a tier has no usable target")
	}
}

func TestNonSmartModelUntouched(t *testing.T) {
	sdc := &failingProvider{mockProvider: mockProvider{name: "sdc", pType: "openai"}}
	e := newSmartEngine(t, sdc)
	req := &canonical.CanonicalRequest{Model: "sdc/sol"}
	ctx := context.Background()
	got, decision, err := e.ApplySmart(ctx, req)
	if err != nil || decision != nil || got != ctx || req.Model != "sdc/sol" {
		t.Fatalf("non-smart request modified: %v %v %q", err, decision, req.Model)
	}
}

func TestSmartRejectsUnknownProvider(t *testing.T) {
	_, err := NewEngine(&config.Config{
		Providers: map[string]config.ProviderConfig{},
		Smart:     config.SmartConfig{Tiers: map[string][]string{"simple": {"ghost/model"}}},
	})
	if err == nil {
		t.Fatal("expected unknown provider error")
	}
}

func TestSmartStatsAndSetSmart(t *testing.T) {
	sdc := &failingProvider{mockProvider: mockProvider{name: "sdc", pType: "openai"}}
	e := newSmartEngine(t, sdc)

	stats := e.GetSmartStats()
	if stats.TotalRequests != 0 {
		t.Fatalf("expected 0 requests, got %d", stats.TotalRequests)
	}

	req := complexRequest()
	ctx, decision, err := e.ApplySmart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if decision.DecisionDuration < 0 {
		t.Fatalf("expected non-negative duration, got %v", decision.DecisionDuration)
	}

	stats = e.GetSmartStats()
	if stats.TotalRequests != 1 || stats.TierCounts["complex"] != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	// Update smart configuration live
	newCfg := config.SmartConfig{
		Classifier: config.ClassifierConfig{Mode: "heuristic"},
		Tiers: map[string][]string{
			"simple":  {"sdc/sol"},
			"complex": {"sdc/sol"},
		},
	}
	persisted, err := e.SetSmart(newCfg)
	if err != nil {
		t.Fatalf("SetSmart failed: %v", err)
	}
	if persisted {
		t.Fatal("expected persisted=false when no SourcePath set")
	}

	masked, hasKey := e.GetSmartMasked()
	if hasKey || masked.Classifier.Mode != "heuristic" {
		t.Fatalf("unexpected masked config: %+v, hasKey=%v", masked, hasKey)
	}

	reqSimple := &canonical.CanonicalRequest{Model: "smart", Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "fix typo"}}}}}
	_, _, err = e.ApplySmart(ctx, reqSimple)
	if err != nil {
		t.Fatalf("ApplySmart with new config failed: %v", err)
	}
	stats = e.GetSmartStats()
	if stats.TotalRequests != 2 || stats.TierCounts["simple"] != 1 {
		t.Fatalf("unexpected stats after second request: %+v", stats)
	}
}
