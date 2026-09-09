package router

import (
	"context"
	"errors"
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
	"github.com/vogler75/babel-gate/pkg/server/trace"
)

type ResolvedRoute struct {
	Provider    providers.Provider
	TargetModel string
}

var ErrTokenCountingUnsupported = errors.New("routed provider does not support token counting")

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
		provider, err := buildProvider(name, pcfg)
		if err != nil {
			return nil, err
		}
		if pcfg.IsEnabled() {
			e.providers[name] = provider
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

func buildProvider(name string, pcfg config.ProviderConfig) (providers.Provider, error) {
	switch strings.ToLower(pcfg.Type) {
	case "openai":
		return openai.NewClient(name, pcfg.APIKey, pcfg.BaseURL, pcfg.EnabledModels, nil), nil
	case "anthropic":
		return anthropic.NewClient(name, pcfg.APIKey, pcfg.BaseURL, pcfg.EnabledModels, nil), nil
	case "google":
		return google.NewClient(name, pcfg.APIKey, pcfg.BaseURL, pcfg.EnabledModels, nil), nil
	case "copilot", "github-copilot":
		return copilot.NewClient(name, pcfg.APIKey, pcfg.BaseURL, pcfg.EnabledModels, nil), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for provider %q", pcfg.Type, name)
	}
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
		return nil, fmt.Errorf("provider %q is disabled or not configured", providerName)
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
			return nil, fmt.Errorf("default route provider %q is disabled or not configured", pName)
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

// ResolveRouteInfo returns the resolved provider name, destination endpoint, and target model.
func (e *Engine) ResolveRouteInfo(requestedModel string) (provider, endpoint, targetModel string) {
	route, err := e.ResolveModel(requestedModel)
	if err != nil || route.Provider == nil {
		return "unknown", "", requestedModel
	}
	return route.Provider.Name(), route.Provider.Endpoint(), route.TargetModel
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

// CountTokens asks the resolved upstream provider to tokenize the translated
// request with the exact target model tokenizer.
func (e *Engine) CountTokens(ctx context.Context, req *canonical.CanonicalRequest) (int, error) {
	if req == nil {
		return 0, fmt.Errorf("request is required")
	}
	route, err := e.ResolveModel(req.Model)
	if err != nil {
		return 0, err
	}
	counter, ok := route.Provider.(providers.TokenCounter)
	if !ok {
		return 0, ErrTokenCountingUnsupported
	}
	targetReq := *req
	targetReq.Model = route.TargetModel
	return counter.CountTokens(ctx, &targetReq)
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
				if tr := trace.FromContext(ctx); tr != nil {
					tr.AddNote(fmt.Sprintf("fallback to %s", fb))
					tr.SetRoute(req.Model, fbRoute.Provider.Name(), fbRoute.Provider.Endpoint(), fbRoute.TargetModel)
				}
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

	ch, err := route.Provider.Stream(ctx, &targetReq)
	if err == nil {
		return ch, nil
	}

	// Check fallbacks
	if fallbacks, ok := e.cfg.Routing.Fallbacks[req.Model]; ok {
		for _, fb := range fallbacks {
			fbRoute, fbErr := e.ResolveModel(fb)
			if fbErr != nil {
				continue
			}
			targetReq.Model = fbRoute.TargetModel
			fbCh, err2 := fbRoute.Provider.Stream(ctx, &targetReq)
			if err2 == nil {
				if tr := trace.FromContext(ctx); tr != nil {
					tr.AddNote(fmt.Sprintf("fallback to %s", fb))
					tr.SetRoute(req.Model, fbRoute.Provider.Name(), fbRoute.Provider.Endpoint(), fbRoute.TargetModel)
				}
				return fbCh, nil
			}
		}
	}

	return nil, err
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

// ProviderState is the dashboard-safe view of a configured provider.
type ProviderState struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
}

// GetProviderStates returns both enabled and disabled configured providers.
func (e *Engine) GetProviderStates() []ProviderState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	states := make([]ProviderState, 0, len(e.cfg.Providers))
	for name, pcfg := range e.cfg.Providers {
		priority := pcfg.Priority
		if priority <= 0 {
			priority = 100
		}
		_, active := e.providers[name]
		states = append(states, ProviderState{Name: name, Type: pcfg.Type, Priority: priority, Enabled: active})
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].Priority != states[j].Priority {
			return states[i].Priority < states[j].Priority
		}
		return states[i].Name < states[j].Name
	})
	return states
}

// SetProviderEnabled applies a provider state immediately and, when a source
// config exists, persists it before changing the running engine.
func (e *Engine) SetProviderEnabled(name string, enabled bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	pcfg, ok := e.cfg.Providers[name]
	if !ok {
		return false, fmt.Errorf("provider %q is not configured", name)
	}
	persisted := e.cfg.SourcePath != ""
	if persisted {
		if err := config.UpdateProviderEnabled(e.cfg.SourcePath, name, enabled); err != nil {
			return false, err
		}
	}
	if enabled {
		provider, err := buildProvider(name, pcfg)
		if err != nil {
			return false, err
		}
		e.providers[name] = provider
	} else {
		delete(e.providers, name)
	}
	pcfg.Enabled = new(bool)
	*pcfg.Enabled = enabled
	e.cfg.Providers[name] = pcfg
	return persisted, nil
}

// GetRouting returns a copy of all mutable routing configuration.
func (e *Engine) GetRouting() config.RoutingConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneRouting(e.cfg.Routing)
}

// SetRouting atomically persists and activates a complete routing definition.
func (e *Engine) SetRouting(routing config.RoutingConfig) (bool, error) {
	if err := validateRouting(routing); err != nil {
		return false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.validateRoutingProvidersLocked(routing); err != nil {
		return false, err
	}
	persisted := e.cfg.SourcePath != ""
	if persisted {
		if err := config.UpdateRouting(e.cfg.SourcePath, routing); err != nil {
			return false, err
		}
	}
	e.cfg.Routing = cloneRouting(routing)
	return persisted, nil
}

// ReloadRouting replaces the running routes with the routing section currently
// stored in the active configuration file. Provider clients are left untouched.
func (e *Engine) ReloadRouting() error {
	e.mu.RLock()
	path := e.cfg.SourcePath
	e.mu.RUnlock()
	routing, err := config.LoadRouting(path)
	if err != nil {
		return err
	}
	if err := validateRouting(routing); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.validateRoutingProvidersLocked(routing); err != nil {
		return err
	}
	e.cfg.Routing = cloneRouting(routing)
	return nil
}

func (e *Engine) validateRoutingProvidersLocked(routing config.RoutingConfig) error {
	targets := make([]string, 0, len(routing.Routes)+1)
	if routing.Default != "" {
		targets = append(targets, routing.Default)
	}
	for _, target := range routing.Routes {
		targets = append(targets, target)
	}
	for _, fallbacks := range routing.Fallbacks {
		targets = append(targets, fallbacks...)
	}
	for _, target := range targets {
		if slash := strings.Index(target, "/"); slash > 0 {
			if _, ok := e.cfg.Providers[target[:slash]]; !ok {
				return fmt.Errorf("route target references unknown provider %q", target[:slash])
			}
		}
	}
	return nil
}

func validateRouting(routing config.RoutingConfig) error {
	for alias, target := range routing.Routes {
		if strings.TrimSpace(alias) == "" || strings.TrimSpace(target) == "" {
			return fmt.Errorf("route aliases and targets cannot be empty")
		}
		if alias == target {
			return fmt.Errorf("route %q cannot target itself", alias)
		}
	}
	for model, fallbacks := range routing.Fallbacks {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("fallback model cannot be empty")
		}
		for _, target := range fallbacks {
			if strings.TrimSpace(target) == "" {
				return fmt.Errorf("fallback targets cannot be empty")
			}
		}
	}
	return nil
}

func cloneRouting(in config.RoutingConfig) config.RoutingConfig {
	out := config.RoutingConfig{Default: in.Default, Routes: make(map[string]string), Fallbacks: make(map[string][]string)}
	for key, value := range in.Routes {
		out.Routes[key] = value
	}
	for key, values := range in.Fallbacks {
		out.Fallbacks[key] = append([]string(nil), values...)
	}
	return out
}
