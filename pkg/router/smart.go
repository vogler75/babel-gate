package router

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/smart"
)

type smartChainKey struct{}

// SmartStats provides aggregate metrics about smart routing decisions.
type SmartStats struct {
	TotalRequests     int64            `json:"total_requests"`
	TierCounts        map[string]int64 `json:"tier_counts"`
	PrimaryDecisions  int64            `json:"primary_decisions"`
	FallbackDecisions int64            `json:"fallback_decisions"`
	StickyReuses      int64            `json:"sticky_reuses"`
	TotalDecisionMs   int64            `json:"total_decision_ms"`
	AvgDecisionMs     float64          `json:"avg_decision_ms"`
}

// IsSmartModel reports whether a requested model should be routed by tier.
func (e *Engine) IsSmartModel(model string) bool {
	return e.smart != nil && smart.IsSmartModel(model)
}

// ApplySmart classifies a smart request and rewrites req.Model to the first
// usable target of its tier. The remaining targets are stored in the returned
// context and become the fallback chain for Execute and Stream. Requests for
// other models are returned unchanged with a nil decision.
func (e *Engine) ApplySmart(ctx context.Context, req *canonical.CanonicalRequest) (context.Context, *smart.Decision, error) {
	if !e.IsSmartModel(req.Model) {
		return ctx, nil, nil
	}
	decision := e.smart.Decide(ctx, req)
	e.recordSmartDecision(&decision)
	targets := e.usableTargets(decision.Targets)
	if len(targets) == 0 {
		return ctx, &decision, fmt.Errorf("smart tier %s has no usable target (configured: %s)", decision.Tier, strings.Join(decision.Targets, ", "))
	}
	decision.Targets = targets
	req.Model = targets[0]

	if tr := trace.FromContext(ctx); tr != nil {
		tr.AddNote(fmt.Sprintf("smart %s: %s", decision.Tier, decision.Reason))
	}
	return context.WithValue(ctx, smartChainKey{}, targets[1:]), &decision, nil
}

func (e *Engine) recordSmartDecision(d *smart.Decision) {
	if d == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.smartStats.TotalRequests++
	if e.smartStats.TierCounts == nil {
		e.smartStats.TierCounts = make(map[string]int64)
	}
	e.smartStats.TierCounts[d.Tier.String()]++
	if d.Sticky {
		e.smartStats.StickyReuses++
	} else if d.FallbackUsed {
		e.smartStats.FallbackDecisions++
	} else {
		e.smartStats.PrimaryDecisions++
	}
	ms := d.DecisionDuration.Milliseconds()
	e.smartStats.TotalDecisionMs += ms
	if e.smartStats.TotalRequests > 0 {
		e.smartStats.AvgDecisionMs = float64(e.smartStats.TotalDecisionMs) / float64(e.smartStats.TotalRequests)
	}
}

// GetSmartConfig returns a copy of the active smart configuration.
func (e *Engine) GetSmartConfig() config.SmartConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneSmartConfig(e.cfg.Smart)
}

// GetSmartMasked returns a copy of the active smart configuration with any API key masked.
func (e *Engine) GetSmartMasked() (config.SmartConfig, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cfg := cloneSmartConfig(e.cfg.Smart)
	hasKey := cfg.Classifier.APIKey != ""
	if hasKey {
		cfg.Classifier.APIKey = "••••••••"
	}
	return cfg, hasKey
}

// GetSmartStats returns a copy of the cumulative smart routing statistics.
func (e *Engine) GetSmartStats() SmartStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	counts := make(map[string]int64, len(e.smartStats.TierCounts))
	for k, v := range e.smartStats.TierCounts {
		counts[k] = v
	}
	return SmartStats{
		TotalRequests:     e.smartStats.TotalRequests,
		TierCounts:        counts,
		PrimaryDecisions:  e.smartStats.PrimaryDecisions,
		FallbackDecisions: e.smartStats.FallbackDecisions,
		StickyReuses:      e.smartStats.StickyReuses,
		TotalDecisionMs:   e.smartStats.TotalDecisionMs,
		AvgDecisionMs:     e.smartStats.AvgDecisionMs,
	}
}

// GetSmartCooldowns returns a map of providers currently cooling down and their remaining seconds.
func (e *Engine) GetSmartCooldowns() map[string]int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	now := time.Now()
	res := make(map[string]int)
	for prov, until := range e.cooldowns {
		if now.Before(until) {
			res[prov] = int(until.Sub(now).Seconds())
		}
	}
	return res
}

// GetProviderCooldown returns the remaining cooldown duration for a provider (0 if not cooling).
func (e *Engine) GetProviderCooldown(provider string) time.Duration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	until, ok := e.cooldowns[provider]
	if !ok {
		return 0
	}
	now := time.Now()
	if now.Before(until) {
		return until.Sub(now)
	}
	return 0
}

// SetSmart atomically persists and activates a new smart configuration.
func (e *Engine) SetSmart(cfg config.SmartConfig) (bool, error) {
	if cfg.Enabled() {
		if err := smart.Validate(cfg); err != nil {
			return false, err
		}
		e.mu.RLock()
		for tier, targets := range cfg.Tiers {
			for _, target := range targets {
				if slash := strings.Index(target, "/"); slash > 0 {
					prov := target[:slash]
					if _, ok := e.cfg.Providers[prov]; !ok {
						e.mu.RUnlock()
						return false, fmt.Errorf("smart tier %q references unknown provider %q", tier, prov)
					}
				}
			}
		}
		// If masked APIKey was passed, preserve existing
		if cfg.Classifier.APIKey == "••••••••" {
			cfg.Classifier.APIKey = e.cfg.Smart.Classifier.APIKey
		}
		e.mu.RUnlock()
	}

	newRouter, err := smart.NewRouter(cfg)
	if err != nil {
		return false, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	persisted := e.cfg.SourcePath != ""
	if persisted {
		if err := config.UpdateSmart(e.cfg.SourcePath, cfg); err != nil {
			return false, err
		}
	}

	e.smart = newRouter
	e.cfg.Smart = cloneSmartConfig(cfg)
	if cfg.CooldownSeconds > 0 {
		e.smartCooldown = time.Duration(cfg.CooldownSeconds) * time.Second
	} else {
		e.smartCooldown = 60 * time.Second
	}
	return persisted, nil
}

// ReloadSmart reloads the smart configuration from the active configuration file.
func (e *Engine) ReloadSmart() error {
	e.mu.RLock()
	path := e.cfg.SourcePath
	e.mu.RUnlock()

	cfg, err := config.LoadSmart(path)
	if err != nil {
		return err
	}
	_, err = e.SetSmart(cfg)
	return err
}

func cloneSmartConfig(s config.SmartConfig) config.SmartConfig {
	res := s
	if s.Tiers != nil {
		res.Tiers = make(map[string][]string, len(s.Tiers))
		for k, v := range s.Tiers {
			res.Tiers[k] = append([]string(nil), v...)
		}
	}
	return res
}

func smartChainFromContext(ctx context.Context) ([]string, bool) {
	chain, ok := ctx.Value(smartChainKey{}).([]string)
	return chain, ok
}

// usableTargets keeps resolvable targets, preferring providers that are not
// cooling down. Cooling providers move to the end rather than disappearing,
// so an outage of every provider still gets one real attempt each.
func (e *Engine) usableTargets(targets []string) []string {
	now := time.Now()
	var ready, cooling []string
	for _, target := range targets {
		route, err := e.ResolveModel(target)
		if err != nil {
			continue
		}
		e.mu.RLock()
		until := e.cooldowns[route.Provider.Name()]
		e.mu.RUnlock()
		if now.Before(until) {
			cooling = append(cooling, target)
		} else {
			ready = append(ready, target)
		}
	}
	return append(ready, cooling...)
}

// noteSmartFailure pauses a provider for smart routing after errors that are
// likely to persist for a while: rate limits, exhausted quota, server errors
// and transport failures. Request-shaped errors (4xx) only skip to the next
// target without penalizing the provider.
func (e *Engine) noteSmartFailure(provider string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	status := errorStatus(err)
	if status != 0 && status != 402 && status != 429 && status < 500 {
		log.Printf("[SMART] %s failed with status %d, trying next target", provider, status)
		return
	}
	e.mu.Lock()
	e.cooldowns[provider] = time.Now().Add(e.smartCooldown)
	e.mu.Unlock()
	log.Printf("[SMART] %s failed (%v), pausing it for %s", provider, truncateErr(err), e.smartCooldown)
}

var statusPattern = regexp.MustCompile(`(?:status|error) (\d{3})\b`)

// errorStatus extracts an upstream HTTP status from a provider error, or 0.
func errorStatus(err error) int {
	var apiErr *providers.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	if m := statusPattern.FindStringSubmatch(err.Error()); m != nil {
		status, _ := strconv.Atoi(m[1])
		return status
	}
	return 0
}

func truncateErr(err error) string {
	msg := err.Error()
	if len(msg) > 200 {
		return msg[:200] + "..."
	}
	return msg
}
