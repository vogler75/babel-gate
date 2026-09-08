package router

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/providers/anthropic"
	"github.com/vogler75/babel-gate/pkg/providers/copilot"
	"github.com/vogler75/babel-gate/pkg/providers/google"
	"github.com/vogler75/babel-gate/pkg/providers/openai"
)

type ResolvedRoute struct {
	Provider    providers.Provider
	TargetModel string
}

type Engine struct {
	mu             sync.RWMutex
	cfg            *config.Config
	providers      map[string]providers.Provider
	providerModels map[string]map[string]bool // providerName -> lowerModelID -> true
}

func NewEngine(cfg *config.Config) (*Engine, error) {
	e := &Engine{
		cfg:            cfg,
		providers:      make(map[string]providers.Provider),
		providerModels: make(map[string]map[string]bool),
	}

	for name, pcfg := range cfg.Providers {
		switch strings.ToLower(pcfg.Type) {
		case "openai":
			client := openai.NewClient(name, pcfg.APIKey, pcfg.BaseURL, pcfg.EnabledModels, nil)
			e.providers[name] = client
		case "anthropic":
			client := anthropic.NewClient(name, pcfg.APIKey, pcfg.BaseURL, pcfg.EnabledModels, nil)
			e.providers[name] = client
		case "google":
			client := google.NewClient(name, pcfg.APIKey, pcfg.BaseURL, pcfg.EnabledModels, nil)
			e.providers[name] = client
		case "copilot", "github-copilot":
			client := copilot.NewClient(name, pcfg.APIKey, pcfg.BaseURL, pcfg.EnabledModels, nil)
			e.providers[name] = client
		default:
			return nil, fmt.Errorf("unsupported provider type %q for provider %q", pcfg.Type, name)
		}

		if len(pcfg.EnabledModels) > 0 {
			e.providerModels[name] = make(map[string]bool, len(pcfg.EnabledModels))
			for _, m := range pcfg.EnabledModels {
				e.providerModels[name][strings.ToLower(m)] = true
			}
		}
	}

	return e, nil
}

// RegisterProvider dynamically registers or replaces a provider.
func (e *Engine) RegisterProvider(p providers.Provider) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.providers[p.Name()] = p
	if e.providerModels == nil {
		e.providerModels = make(map[string]map[string]bool)
	}
	if pcfg, ok := e.cfg.Providers[p.Name()]; ok && len(pcfg.EnabledModels) > 0 {
		if e.providerModels[p.Name()] == nil {
			e.providerModels[p.Name()] = make(map[string]bool)
		}
		for _, m := range pcfg.EnabledModels {
			e.providerModels[p.Name()][strings.ToLower(m)] = true
		}
	}
}

// SetProviderModels updates the cached model IDs for a provider.
func (e *Engine) SetProviderModels(providerName string, modelIDs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.providerModels == nil {
		e.providerModels = make(map[string]map[string]bool)
	}
	modelsMap := make(map[string]bool, len(modelIDs))
	for _, m := range modelIDs {
		modelsMap[strings.ToLower(m)] = true
	}
	e.providerModels[providerName] = modelsMap
}

// SyncProviderModels caches the models discovered from a provider.
func (e *Engine) SyncProviderModels(providerName string, models []providers.ModelInfo) {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	e.SetProviderModels(providerName, ids)
}

// GetProviderModels returns the cached model IDs for a provider.
func (e *Engine) GetProviderModels(providerName string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.providerModels == nil {
		return nil
	}
	modelsMap := e.providerModels[providerName]
	if modelsMap == nil {
		return nil
	}
	res := make([]string, 0, len(modelsMap))
	for m := range modelsMap {
		res = append(res, m)
	}
	sort.Strings(res)
	return res
}

func (e *Engine) providerHasModel(providerName, modelID string) bool {
	if e.providerModels == nil {
		return false
	}
	modelsMap, ok := e.providerModels[providerName]
	if !ok {
		return false
	}
	return modelsMap[strings.ToLower(modelID)]
}

type providerWithPriority struct {
	name     string
	priority int
	provider providers.Provider
}

// getSortedProviders returns providers sorted by priority ascending (prio 1, prio 2, ...).
func (e *Engine) getSortedProviders() []providers.Provider {
	var list []providerWithPriority
	for name, p := range e.providers {
		prio := 100
		if e.cfg != nil {
			if pcfg, ok := e.cfg.Providers[name]; ok && pcfg.Priority > 0 {
				prio = pcfg.Priority
			}
		}
		list = append(list, providerWithPriority{
			name:     name,
			priority: prio,
			provider: p,
		})
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].priority != list[j].priority {
			return list[i].priority < list[j].priority
		}
		return list[i].name < list[j].name
	})

	out := make([]providers.Provider, len(list))
	for i, item := range list {
		out[i] = item.provider
	}
	return out
}

// GetProviderPriority returns the priority for a provider (defaults to 100).
func (e *Engine) GetProviderPriority(name string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.cfg != nil {
		if pcfg, ok := e.cfg.Providers[name]; ok && pcfg.Priority > 0 {
			return pcfg.Priority
		}
	}
	return 100
}

// ResolveModel maps an incoming requested model string to a Provider and target model name.
func (e *Engine) ResolveModel(requestedModel string) (*ResolvedRoute, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	target := requestedModel

	// 1. Check alias routes
	if e.cfg != nil {
		if mapped, exists := e.cfg.Routing.Routes[target]; exists && mapped != "" {
			target = mapped
		}
	}

	// 2. Check prefix (e.g. "google/gemini-2.5-pro", "openai/gpt-4o", "anthropic/claude-3-7-sonnet")
	if slashIdx := strings.Index(target, "/"); slashIdx != -1 {
		providerName := target[:slashIdx]
		modelName := target[slashIdx+1:]
		if p, ok := e.providers[providerName]; ok {
			return &ResolvedRoute{
				Provider:    p,
				TargetModel: modelName,
			}, nil
		}
	}

	// 3. Priority-based model lookup: check priority 1, then priority 2, etc.
	sorted := e.getSortedProviders()
	for _, p := range sorted {
		if e.providerHasModel(p.Name(), target) {
			return &ResolvedRoute{
				Provider:    p,
				TargetModel: target,
			}, nil
		}
	}

	// 4. Fallback by family prefix if model was not in cached catalog
	for _, p := range sorted {
		pType := strings.ToLower(p.Type())
		if strings.HasPrefix(target, "claude-") && pType == "anthropic" {
			return &ResolvedRoute{Provider: p, TargetModel: target}, nil
		}
		if strings.HasPrefix(target, "gemini-") && pType == "google" {
			return &ResolvedRoute{Provider: p, TargetModel: target}, nil
		}
		if (strings.HasPrefix(target, "gpt-") || strings.HasPrefix(target, "o1") || strings.HasPrefix(target, "o3")) &&
			(pType == "openai" || pType == "copilot") {
			return &ResolvedRoute{Provider: p, TargetModel: target}, nil
		}
	}

	// 5. Check if single provider is registered
	if len(e.providers) == 1 {
		for _, p := range e.providers {
			return &ResolvedRoute{Provider: p, TargetModel: target}, nil
		}
	}

	// 6. Default route fallback
	if e.cfg != nil && e.cfg.Routing.Default != "" {
		def := e.cfg.Routing.Default
		if slashIdx := strings.Index(def, "/"); slashIdx != -1 {
			pName := def[:slashIdx]
			mName := def[slashIdx+1:]
			if p, ok := e.providers[pName]; ok {
				return &ResolvedRoute{Provider: p, TargetModel: mName}, nil
			}
		}
		for _, p := range e.providers {
			return &ResolvedRoute{Provider: p, TargetModel: def}, nil
		}
	}

	return nil, fmt.Errorf("unable to resolve model %q to any active provider", requestedModel)
}

// ResolveTrackingModel returns the resolved provider name and the provider-prefixed model name.
// For example, if a model without a provider ("gemini-3.5-flash") resolves to provider "copilot",
// it returns ("copilot", "copilot/gemini-3.5-flash").
// If requestedModel already has a provider prefix matching the resolved provider, it is returned as-is.
// If requestedModel is an alias (e.g. "fast" -> "google/gemini-2.5-flash"),
// it returns ("google", "google/gemini-2.5-flash").
func (e *Engine) ResolveTrackingModel(requestedModel string) (string, string) {
	route, err := e.ResolveModel(requestedModel)
	if err != nil || route.Provider == nil {
		return "unknown", requestedModel
	}
	provName := route.Provider.Name()
	cleanModel := strings.TrimPrefix(route.TargetModel, provName+"/")
	return provName, provName + "/" + cleanModel
}

// ResolveProviderName returns the resolved provider name for a model, or "unknown" if unresolved.
func (e *Engine) ResolveProviderName(model string) string {
	prov, _ := e.ResolveTrackingModel(model)
	return prov
}

func (e *Engine) findProviderByType(pType string) providers.Provider {
	for _, p := range e.providers {
		if p.Type() == pType {
			return p
		}
	}
	return nil
}

// Execute routes a non-streaming canonical request to the appropriate upstream provider.
func (e *Engine) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	route, err := e.ResolveModel(req.Model)
	if err != nil {
		return nil, err
	}

	targetReq := *req
	targetReq.Model = route.TargetModel

	resp, err := route.Provider.Execute(ctx, &targetReq)
	if err == nil {
		return resp, nil
	}

	// Check fallbacks
	if fallbacks, ok := e.cfg.Routing.Fallbacks[req.Model]; ok {
		for _, fb := range fallbacks {
			fbRoute, fbErr := e.ResolveModel(fb)
			if fbErr != nil {
				continue
			}
			targetReq.Model = fbRoute.TargetModel
			fbResp, err2 := fbRoute.Provider.Execute(ctx, &targetReq)
			if err2 == nil {
				return fbResp, nil
			}
		}
	}

	return nil, err
}

// Stream routes a streaming canonical request to the appropriate upstream provider.
func (e *Engine) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	route, err := e.ResolveModel(req.Model)
	if err != nil {
		return nil, err
	}

	targetReq := *req
	targetReq.Model = route.TargetModel

	return route.Provider.Stream(ctx, &targetReq)
}

// GetProviders returns all registered providers.
func (e *Engine) GetProviders() map[string]providers.Provider {
	e.mu.RLock()
	defer e.mu.RUnlock()
	res := make(map[string]providers.Provider, len(e.providers))
	for k, v := range e.providers {
		res[k] = v
	}
	return res
}

// GetRoutes returns configured alias routes.
func (e *Engine) GetRoutes() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	res := make(map[string]string, len(e.cfg.Routing.Routes))
	for k, v := range e.cfg.Routing.Routes {
		res[k] = v
	}
	return res
}
