package router

import (
	"context"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers"
)

type mockProvider struct {
	name   string
	pType  string
	models []providers.ModelInfo
}

func (m *mockProvider) Name() string     { return m.name }
func (m *mockProvider) Type() string     { return m.pType }
func (m *mockProvider) Endpoint() string { return "http://mock" }
func (m *mockProvider) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	return &canonical.CanonicalResponse{
		ID:    "resp-1",
		Model: req.Model,
		Message: canonical.Message{
			Role: canonical.RoleAssistant,
			Parts: []canonical.ContentPart{
				{Type: canonical.PartText, Text: "mock response from " + m.name},
			},
		},
	}, nil
}
func (m *mockProvider) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	ch := make(chan canonical.CanonicalEvent, 2)
	ch <- canonical.CanonicalEvent{Type: canonical.EventTextDelta, Text: "chunk"}
	ch <- canonical.CanonicalEvent{Type: canonical.EventMessageDone}
	close(ch)
	return ch, nil
}
func (m *mockProvider) ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	return m.models, nil
}

func TestResolveModel(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			Routes: map[string]string{
				"claude-3-7-sonnet": "google/gemini-2.5-pro",
				"gpt-4o":            "anthropic/claude-3-7-sonnet-20250219",
			},
			Default: "google/gemini-2.5-flash",
		},
	}

	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("unexpected error creating engine: %v", err)
	}

	mockGoogle := &mockProvider{
		name:   "google",
		pType:  "google",
		models: []providers.ModelInfo{{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro"}},
	}
	mockAnthropic := &mockProvider{
		name:   "anthropic",
		pType:  "anthropic",
		models: []providers.ModelInfo{{ID: "claude-3-7-sonnet-20250219", Name: "Claude 3.7 Sonnet"}},
	}
	mockOpenAI := &mockProvider{
		name:   "openai",
		pType:  "openai",
		models: []providers.ModelInfo{{ID: "o3-mini", Name: "O3 Mini"}},
	}

	engine.RegisterProvider(mockGoogle)
	engine.RegisterProvider(mockAnthropic)
	engine.RegisterProvider(mockOpenAI)

	// 1. Test alias resolution: claude-3-7-sonnet -> google/gemini-2.5-pro
	route, err := engine.ResolveModel("claude-3-7-sonnet")
	if err != nil {
		t.Fatalf("failed to resolve claude-3-7-sonnet: %v", err)
	}
	if route.Provider.Name() != "google" || route.TargetModel != "gemini-2.5-pro" {
		t.Errorf("expected google/gemini-2.5-pro, got %s/%s", route.Provider.Name(), route.TargetModel)
	}

	// 2. Test explicit prefix resolution
	route2, err := engine.ResolveModel("anthropic/claude-3-7-sonnet-20250219")
	if err != nil {
		t.Fatalf("failed to resolve explicit prefix: %v", err)
	}
	if route2.Provider.Name() != "anthropic" || route2.TargetModel != "claude-3-7-sonnet-20250219" {
		t.Errorf("expected anthropic/claude-3-7-sonnet-20250219, got %s/%s", route2.Provider.Name(), route2.TargetModel)
	}

	// 3. Test type prefix inference
	route3, err := engine.ResolveModel("gemini-2.5-pro")
	if err != nil {
		t.Fatalf("failed to resolve gemini- prefix: %v", err)
	}
	if route3.Provider.Name() != "google" {
		t.Errorf("expected google provider, got %s", route3.Provider.Name())
	}

	// 4. Test default route
	route4, err := engine.ResolveModel("unknown-model")
	if err != nil {
		t.Fatalf("failed to resolve fallback default: %v", err)
	}

	// 5. Test Copilot resolution
	mockCopilot := &mockProvider{
		name:   "copilot",
		pType:  "copilot",
		models: []providers.ModelInfo{{ID: "gpt-4o", Name: "GPT-4o"}},
	}
	engine.RegisterProvider(mockCopilot)
	route5, err := engine.ResolveModel("copilot/gpt-4o")
	if err != nil {
		t.Fatalf("failed to resolve copilot/gpt-4o: %v", err)
	}
	if route5.Provider.Name() != "copilot" || route5.TargetModel != "gpt-4o" {
		t.Errorf("expected copilot/gpt-4o, got %s/%s", route5.Provider.Name(), route5.TargetModel)
	}
	if route4.Provider.Name() != "google" || route4.TargetModel != "gemini-2.5-flash" {
		t.Errorf("expected default route google/gemini-2.5-flash, got %s/%s", route4.Provider.Name(), route4.TargetModel)
	}
}

func TestCatalogList(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			Routes: map[string]string{
				"custom-coder": "google/gemini-2.5-pro",
			},
		},
	}
	engine, _ := NewEngine(cfg)
	engine.RegisterProvider(&mockProvider{
		name:   "google",
		pType:  "google",
		models: []providers.ModelInfo{{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro"}},
	})

	catalog := NewCatalog(engine)
	models, err := catalog.ListAll(context.Background())
	if err != nil {
		t.Fatalf("failed to list models: %v", err)
	}

	openaiFmt := catalog.FormatOpenAI(models)
	if openaiFmt["object"] != "list" {
		t.Errorf("expected object list in openai format")
	}

	anthropicFmt := catalog.FormatAnthropic(models)
	if anthropicFmt["data"] == nil {
		t.Errorf("expected data array in anthropic format")
	}

	googleFmt := catalog.FormatGoogle(models)
	if googleFmt["models"] == nil {
		t.Errorf("expected models array in google format")
	}
	if len(models) < 2 {
		t.Errorf("expected at least 2 catalog models, got %d", len(models))
	}
}

func TestCatalogOrder(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"copilot":   {Type: "copilot", Priority: 4},
			"google":    {Type: "google", Priority: 1},
			"anthropic": {Type: "anthropic", Priority: 2},
		},
		Routing: config.RoutingConfig{
			Routes: map[string]string{
				"z-alias": "google/gemini-2.5-flash",
				"a-alias": "google/gemini-2.5-flash",
			},
		},
	}
	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	engine.RegisterProvider(&mockProvider{
		name:  "copilot",
		pType: "copilot",
		models: []providers.ModelInfo{
			{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash"},
			{ID: "claude-opus-4.8", Name: "Claude Opus 4.8"},
		},
	})
	engine.RegisterProvider(&mockProvider{
		name:  "google",
		pType: "google",
		models: []providers.ModelInfo{
			{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro"},
			{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash"},
		},
	})
	engine.RegisterProvider(&mockProvider{
		name:  "anthropic",
		pType: "anthropic",
		models: []providers.ModelInfo{
			{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5"},
			{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5"},
		},
	})

	catalog := NewCatalog(engine)
	models, err := catalog.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}

	expectedIDs := []string{
		// Google (prio 1), sorted alphabetically
		"gemini-2.5-flash",
		"gemini-2.5-pro",
		// Anthropic (prio 2), sorted alphabetically
		"claude-haiku-4-5",
		"claude-sonnet-4-5",
		// Copilot (prio 4), sorted alphabetically
		"claude-opus-4.8",
		"gemini-3.8-flash",
		// Aliases (prio 999), sorted alphabetically
		"a-alias",
		"z-alias",
	}

	if len(models) != len(expectedIDs) {
		t.Fatalf("expected %d models, got %d", len(expectedIDs), len(models))
	}

	for i, expected := range expectedIDs {
		if models[i].ID != expected {
			t.Errorf("at index %d: expected %q, got %q (provider: %s)", i, expected, models[i].ID, models[i].Provider)
		}
	}
}

func TestPriorityBasedModelResolution(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"google-primary": {
				Type:     "google",
				Priority: 1,
			},
			"copilot-secondary": {
				Type:     "copilot",
				Priority: 2,
			},
		},
	}

	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	mockP1 := &mockProvider{
		name:   "google-primary",
		pType:  "google",
		models: []providers.ModelInfo{
			{ID: "gemini-2.5-flash-lite", Name: "Gemini 2.5 Flash Lite"},
			{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro"},
		},
	}
	mockP2 := &mockProvider{
		name:   "copilot-secondary",
		pType:  "copilot",
		models: []providers.ModelInfo{
			{ID: "gemini-2.5-flash-lite", Name: "Gemini 2.5 Flash Lite"},
			{ID: "gpt-4o", Name: "GPT-4o"},
		},
	}

	engine.RegisterProvider(mockP1)
	engine.SyncProviderModels("google-primary", mockP1.models)

	engine.RegisterProvider(mockP2)
	engine.SyncProviderModels("copilot-secondary", mockP2.models)

	// 1. Both host gemini-2.5-flash-lite; Prio 1 (google-primary) must win
	route, err := engine.ResolveModel("gemini-2.5-flash-lite")
	if err != nil {
		t.Fatalf("failed to resolve gemini-2.5-flash-lite: %v", err)
	}
	if route.Provider.Name() != "google-primary" {
		t.Fatalf("expected google-primary (Prio 1) to win, got %s", route.Provider.Name())
	}

	// 2. Only copilot-secondary hosts gpt-4o; must resolve to copilot-secondary
	routeGPT, err := engine.ResolveModel("gpt-4o")
	if err != nil {
		t.Fatalf("failed to resolve gpt-4o: %v", err)
	}
	if routeGPT.Provider.Name() != "copilot-secondary" {
		t.Fatalf("expected copilot-secondary, got %s", routeGPT.Provider.Name())
	}

	// 3. Explicit prefix overrides priority
	routeExplicit, err := engine.ResolveModel("copilot-secondary/gemini-2.5-flash-lite")
	if err != nil {
		t.Fatalf("failed to resolve explicit prefix: %v", err)
	}
	if routeExplicit.Provider.Name() != "copilot-secondary" {
		t.Fatalf("expected copilot-secondary, got %s", routeExplicit.Provider.Name())
	}

	// 4. Invert priorities: copilot-secondary is Prio 1, google-primary is Prio 2
	cfg.Providers["google-primary"] = config.ProviderConfig{Type: "google", Priority: 2}
	cfg.Providers["copilot-secondary"] = config.ProviderConfig{Type: "copilot", Priority: 1}

	routeInverted, err := engine.ResolveModel("gemini-2.5-flash-lite")
	if err != nil {
		t.Fatalf("failed to resolve gemini-2.5-flash-lite after inverting prio: %v", err)
	}
	if routeInverted.Provider.Name() != "copilot-secondary" {
		t.Fatalf("expected copilot-secondary (new Prio 1) to win, got %s", routeInverted.Provider.Name())
	}
}

func TestCatalogDuplicateModelAcrossProviders(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"google":  {Type: "google", Priority: 1},
			"copilot": {Type: "copilot", Priority: 2},
		},
		Routing: config.RoutingConfig{
			Routes: map[string]string{
				"gemini-3.5-flash": "copilot/gemini-3.5-flash",
			},
		},
	}
	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	engine.RegisterProvider(&mockProvider{
		name:  "google",
		pType: "google",
		models: []providers.ModelInfo{
			{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash"},
			// Duplicate within the same provider should be deduplicated
			{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash Duplicate"},
		},
	})

	engine.RegisterProvider(&mockProvider{
		name:  "copilot",
		pType: "copilot",
		models: []providers.ModelInfo{
			{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash (Copilot)"},
			{ID: "gpt-4o", Name: "GPT-4o"},
		},
	})

	catalog := NewCatalog(engine)
	models, err := catalog.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}

	// Should contain:
	// 1. Google gemini-3.5-flash (native, prio 1)
	// 2. Copilot gemini-3.5-flash (native, prio 2)
	// 3. Copilot gpt-4o (native, prio 2)
	// 4. gemini-3.5-flash alias (alias, prio 999)
	if len(models) != 4 {
		t.Fatalf("expected 4 models in catalog, got %d: %+v", len(models), models)
	}

	var foundGoogle, foundCopilot, foundAlias bool
	for _, m := range models {
		if m.ID == "gemini-3.5-flash" {
			switch m.Provider {
			case "google":
				foundGoogle = true
			case "copilot":
				foundCopilot = true
			case "router-alias":
				foundAlias = true
			}
		}
	}

	if !foundGoogle {
		t.Errorf("expected gemini-3.5-flash for provider google in catalog")
	}
	if !foundCopilot {
		t.Errorf("expected gemini-3.5-flash for provider copilot in catalog")
	}
	if !foundAlias {
		t.Errorf("expected gemini-3.5-flash alias in catalog")
	}
}

func TestResolveTrackingModel(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"google-primary": {Type: "google", Priority: 1},
			"copilot-secondary": {Type: "copilot", Priority: 2},
		},
		Routing: config.RoutingConfig{
			Routes: map[string]string{
				"fast": "google-primary/gemini-2.5-flash",
			},
		},
	}
	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	engine.RegisterProvider(&mockProvider{
		name:  "google-primary",
		pType: "google",
		models: []providers.ModelInfo{
			{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash"},
		},
	})
	engine.SyncProviderModels("google-primary", []providers.ModelInfo{{ID: "gemini-2.5-flash"}})

	engine.RegisterProvider(&mockProvider{
		name:  "copilot-secondary",
		pType: "copilot",
		models: []providers.ModelInfo{
			{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash"},
			{ID: "gpt-4o", Name: "GPT-4o"},
		},
	})
	engine.SyncProviderModels("copilot-secondary", []providers.ModelInfo{
		{ID: "gemini-2.5-flash"},
		{ID: "gpt-4o"},
	})

	// 1. Request without provider: matches highest priority (google-primary)
	prov1, track1 := engine.ResolveTrackingModel("gemini-2.5-flash")
	if prov1 != "google-primary" || track1 != "google-primary/gemini-2.5-flash" {
		t.Errorf("expected google-primary and google-primary/gemini-2.5-flash, got %q, %q", prov1, track1)
	}

	// 2. Request with explicit provider prefix
	prov2, track2 := engine.ResolveTrackingModel("copilot-secondary/gemini-2.5-flash")
	if prov2 != "copilot-secondary" || track2 != "copilot-secondary/gemini-2.5-flash" {
		t.Errorf("expected copilot-secondary and copilot-secondary/gemini-2.5-flash, got %q, %q", prov2, track2)
	}

	// 3. Request unique model on secondary provider
	prov3, track3 := engine.ResolveTrackingModel("gpt-4o")
	if prov3 != "copilot-secondary" || track3 != "copilot-secondary/gpt-4o" {
		t.Errorf("expected copilot-secondary and copilot-secondary/gpt-4o, got %q, %q", prov3, track3)
	}

	// 4. Request alias
	prov4, track4 := engine.ResolveTrackingModel("fast")
	if prov4 != "google-primary" || track4 != "google-primary/gemini-2.5-flash" {
		t.Errorf("expected google-primary and google-primary/gemini-2.5-flash, got %q, %q", prov4, track4)
	}
}


