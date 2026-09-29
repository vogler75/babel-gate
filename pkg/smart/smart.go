// Package smart implements a virtual provider that classifies each request
// into a complexity tier and dispatches it to that tier's ordered targets.
package smart

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vogler75/babel-gate/pkg/budget"
	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/classifier"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/session"
)

const DefaultModel = "smart-router"

// Resolver maps a "provider/model" target to a live provider.
type Resolver func(target string) (providers.Provider, string, error)

// Decision is a record of one routing choice, kept for the dashboard.
type Decision struct {
	Time     time.Time         `json:"time"`
	Session  string            `json:"session,omitempty"`
	Result   classifier.Result `json:"result"`
	Tier     classifier.Tier   `json:"tier"`
	Target   string            `json:"target"`
	Pinned   bool              `json:"pinned"`
	Skipped  []string          `json:"skipped,omitempty"`
	Ask      string            `json:"ask,omitempty"`
	ErrorMsg string            `json:"error,omitempty"`
}

// UsageEntry is one line of the usage log.
type UsageEntry struct {
	TS         float64 `json:"ts"`
	OK         bool    `json:"ok"`
	Tier       string  `json:"tier"`
	Source     string  `json:"source,omitempty"`
	Target     string  `json:"target,omitempty"`
	Provider   string  `json:"provider,omitempty"`
	Model      string  `json:"model,omitempty"`
	In         int     `json:"in"`
	Out        int     `json:"out"`
	CacheRead  int     `json:"cache_read"`
	CacheWrite int     `json:"cache_write"`
	Cost       float64 `json:"cost"`
	Sec        float64 `json:"sec"`
	Error      string  `json:"error,omitempty"`
}

type pin struct {
	tier    classifier.Tier
	expires time.Time
}

type health struct {
	fails         int
	cooldownUntil time.Time
}

type settings struct {
	cfg          config.SmartConfig
	classifier   classifier.Classifier
	tiers        map[classifier.Tier][]string
	affinityTTL  time.Duration
	allowedFails int
	cooldown     time.Duration
}

type Router struct {
	model   string
	resolve Resolver
	now     func() time.Time

	mu        sync.Mutex
	set       *settings
	budget    *budget.Tracker
	pins      map[string]pin
	health    map[string]*health
	decisions []Decision

	logMu   sync.Mutex
	logPath string
}

func BuildClassifier(cfg config.SmartConfig) classifier.Classifier {
	var chain []classifier.Classifier
	rules := make([]classifier.KeywordRule, 0, len(cfg.Keywords))
	for _, k := range cfg.Keywords {
		rules = append(rules, classifier.KeywordRule{Keywords: k.Keywords, Tier: k.Tier})
	}
	if len(rules) == 0 {
		rules = classifier.DefaultKeywordRules()
	}
	chain = append(chain, &classifier.Keywords{Rules: rules})

	c := cfg.Classifier
	timeout := time.Duration(c.TimeoutMs) * time.Millisecond
	switch strings.ToLower(c.Type) {
	case "laya":
		chain = append(chain, classifier.NewLaya(c.URL, c.APIKey, c.Model, timeout, c.MinProb, c.MaxChars, c.CacheSize))
	case "auto", "":
		if c.URL != "" && !strings.HasPrefix(c.URL, "${") {
			chain = append(chain, classifier.NewLaya(c.URL, c.APIKey, c.Model, timeout, c.MinProb, c.MaxChars, c.CacheSize))
		}
	case "http":
		chain = append(chain, classifier.NewHTTP(c.URL, c.APIKey, timeout, c.MinProb))
	}
	chain = append(chain, &classifier.Heuristic{})

	def, _ := classifier.ParseTier(cfg.DefaultTier)
	minTier, _ := classifier.ParseTier(cfg.MinTier)
	return &classifier.Chain{Classifiers: chain, Default: def, MinTier: minTier}
}

func buildSettings(cfg config.SmartConfig) *settings {
	s := &settings{
		cfg:          cfg,
		classifier:   BuildClassifier(cfg),
		tiers:        make(map[classifier.Tier][]string),
		affinityTTL:  time.Duration(cfg.SessionAffinityTTLSeconds) * time.Second,
		allowedFails: cfg.AllowedFails,
		cooldown:     time.Duration(cfg.CooldownSeconds) * time.Second,
	}
	if cfg.SessionAffinityTTLSeconds == 0 {
		s.affinityTTL = time.Hour
	}
	if s.allowedFails <= 0 {
		s.allowedFails = 2
	}
	if s.cooldown <= 0 {
		s.cooldown = 5 * time.Minute
	}
	for name, targets := range cfg.Tiers {
		if tier, ok := classifier.ParseTier(name); ok {
			s.tiers[tier] = append([]string(nil), targets...)
		}
	}
	return s
}

func New(cfg config.SmartConfig, resolve Resolver) *Router {
	r := &Router{
		model:   cfg.Model,
		resolve: resolve,
		now:     time.Now,
		set:     buildSettings(cfg),
		pins:    make(map[string]pin),
		health:  make(map[string]*health),
	}
	if r.model == "" {
		r.model = DefaultModel
	}
	return r
}

// Update swaps in new tiers, classifier and failover settings. The model
// name, session pins, target health and recorded spend are kept.
func (r *Router) Update(cfg config.SmartConfig) {
	s := buildSettings(cfg)
	r.mu.Lock()
	r.set = s
	tracker := r.budget
	r.mu.Unlock()
	tracker.SetBudgets(cfg.Budgets)
}

// Config returns the active smart configuration.
func (r *Router) Config() config.SmartConfig {
	return r.settings().cfg
}

func (r *Router) settings() *settings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set
}

func (r *Router) Model() string { return r.model }

func (r *Router) Name() string     { return r.model }
func (r *Router) Type() string     { return "smart" }
func (r *Router) Endpoint() string { return "smart://" + r.model }

func (r *Router) ListModels(context.Context) ([]providers.ModelInfo, error) {
	return []providers.ModelInfo{{ID: r.model, Name: "Smart Router (auto)", Provider: r.model, Description: "Routes by request complexity"}}, nil
}

// tierOrder lists tier, then every higher tier, then lower tiers.
func tierOrder(tier classifier.Tier) []classifier.Tier {
	idx := tier.Index()
	order := append([]classifier.Tier(nil), classifier.Tiers[idx:]...)
	for i := idx - 1; i >= 0; i-- {
		order = append(order, classifier.Tiers[i])
	}
	return order
}

// candidates returns the ordered targets for tier, then the targets of every
// higher tier, then lower tiers, so a request is never dropped while any
// target is healthy.
func (s *settings) candidates(tier classifier.Tier) []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range tierOrder(tier) {
		for _, target := range s.tiers[t] {
			if !seen[target] {
				seen[target] = true
				out = append(out, target)
			}
		}
	}
	return out
}

func (r *Router) candidates(tier classifier.Tier) []string {
	return r.settings().candidates(tier)
}

// tierOf returns the first tier (in candidate order for requested) that
// lists target; spend is charged against that tier's share.
func (s *settings) tierOf(requested classifier.Tier, target string) classifier.Tier {
	for _, t := range tierOrder(requested) {
		for _, x := range s.tiers[t] {
			if x == target {
				return t
			}
		}
	}
	return requested
}

func (r *Router) decide(ctx context.Context, s *settings, req *canonical.CanonicalRequest) (classifier.Result, classifier.Tier, bool, string) {
	ask := classifier.ExtractAsk(req)
	res, _ := s.classifier.Classify(ctx, ask)
	tier := res.Tier
	pinned := false
	if req.SessionID != "" && s.affinityTTL > 0 {
		now := r.now()
		r.mu.Lock()
		if p, ok := r.pins[req.SessionID]; ok && now.Before(p.expires) && p.tier.Index() > tier.Index() {
			tier, pinned = p.tier, true
		}
		r.pins[req.SessionID] = pin{tier: tier, expires: now.Add(s.affinityTTL)}
		if len(r.pins) > 10000 {
			for k, v := range r.pins {
				if now.After(v.expires) {
					delete(r.pins, k)
				}
			}
		}
		r.mu.Unlock()
	}
	return res, tier, pinned, ask
}

func (r *Router) available(target string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.health[target]
	return !ok || !r.now().Before(h.cooldownUntil)
}

func (r *Router) report(s *settings, target string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.health[target]
	if !ok {
		h = &health{}
		r.health[target] = h
	}
	if err == nil {
		h.fails = 0
		return
	}
	h.fails++
	if h.fails >= s.allowedFails {
		h.cooldownUntil = r.now().Add(s.cooldown)
		h.fails = 0
		log.Printf("[SMART] %s cooling down for %s after error: %v", target, s.cooldown, err)
	}
}

// Cooldowns returns targets currently cooling down and when they recover.
func (r *Router) Cooldowns() map[string]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make(map[string]time.Time)
	for target, h := range r.health {
		if now.Before(h.cooldownUntil) {
			out[target] = h.cooldownUntil
		}
	}
	return out
}

func (r *Router) record(d Decision) {
	if len(d.Ask) > 160 {
		d.Ask = d.Ask[:160]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, d)
	if len(r.decisions) > 200 {
		r.decisions = r.decisions[len(r.decisions)-200:]
	}
}

// Decisions returns the most recent routing decisions, newest last.
func (r *Router) Decisions() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Decision(nil), r.decisions...)
}

// SetBudget attaches a spend tracker. Targets whose provider has exhausted
// the budget for the target's tier are skipped.
func (r *Router) SetBudget(b *budget.Tracker) {
	r.mu.Lock()
	r.budget = b
	r.mu.Unlock()
}

func (r *Router) Budget() *budget.Tracker {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.budget
}

// SetUsageLog enables appending one JSON line per smart request to path.
func (r *Router) SetUsageLog(path string) {
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	r.logMu.Lock()
	r.logPath = path
	r.logMu.Unlock()
}

// UsageLog returns the absolute path of the usage log, or "" when disabled.
func (r *Router) UsageLog() string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return r.logPath
}

func (r *Router) logUsage(e UsageEntry) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if r.logPath == "" {
		return
	}
	if dir := filepath.Dir(r.logPath); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	f, err := os.OpenFile(r.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

func dispatch[T any](r *Router, ctx context.Context, req *canonical.CanonicalRequest, call func(providers.Provider, *canonical.CanonicalRequest, func(canonical.Usage)) (T, error)) (T, error) {
	var zero T
	start := r.now()
	s := r.settings()
	res, tier, pinned, ask := r.decide(ctx, s, req)
	d := Decision{Time: start, Session: req.SessionID, Result: res, Tier: tier, Pinned: pinned, Ask: ask}
	tracker := r.Budget()
	fail := func(err error) (T, error) {
		d.ErrorMsg = err.Error()
		r.record(d)
		r.logUsage(UsageEntry{TS: unixSeconds(start), Tier: string(tier), Source: res.Source, Sec: r.now().Sub(start).Seconds(), Error: err.Error()})
		return zero, err
	}

	targets := s.candidates(tier)
	if len(targets) == 0 {
		return fail(fmt.Errorf("smart router: no targets configured"))
	}
	need := requiredContext(req)
	var lastErr error
	var cooling, tooSmall []string
	for pass := 0; pass < 3; pass++ {
		list := targets
		switch pass {
		case 1:
			list = cooling
		case 2:
			list = tooSmall
		}
		for _, target := range list {
			if pass == 0 {
				if w := s.cfg.ContextWindows[target]; w > 0 && need > w {
					tooSmall = append(tooSmall, target)
					d.Skipped = append(d.Skipped, fmt.Sprintf("%s (context %d>%d)", target, need, w))
					continue
				}
				if !r.available(target) {
					cooling = append(cooling, target)
					d.Skipped = append(d.Skipped, target+" (cooldown)")
					continue
				}
			}
			prov, model, err := r.resolve(target)
			if err != nil {
				d.Skipped = append(d.Skipped, target+" (unavailable)")
				lastErr = err
				continue
			}
			chargeTier := string(s.tierOf(tier, target))
			if !tracker.Allowed(prov.Name(), chargeTier) {
				d.Skipped = append(d.Skipped, target+" (budget)")
				continue
			}
			targetReq := *req
			targetReq.Model = model
			provName := prov.Name()
			onUsage := func(u canonical.Usage) {
				cost := tracker.Record(provName, model, chargeTier, u)
				r.logUsage(UsageEntry{
					TS: unixSeconds(start), OK: true, Tier: string(tier), Source: res.Source, Target: target,
					Provider: provName, Model: model, In: u.PromptTokens, Out: u.CompletionTokens,
					CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens,
					Cost: cost, Sec: r.now().Sub(start).Seconds(),
				})
			}
			out, err := call(prov, &targetReq, onUsage)
			r.report(s, target, err)
			if err != nil {
				d.Skipped = append(d.Skipped, target+" (error)")
				lastErr = err
				if ctx.Err() != nil {
					break
				}
				continue
			}
			d.Target = target
			r.record(d)
			if tr := trace.FromContext(ctx); tr != nil {
				tr.SetRoute(req.Model, prov.Name(), prov.Endpoint(), model)
				tr.AddNote(fmt.Sprintf("smart %s via %s", tier, res.Source))
			}
			log.Printf("[SMART] tier=%s source=%s conf=%.2f pinned=%v -> %s", tier, res.Source, res.Confidence, pinned, target)
			return out, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("smart router: all targets are unavailable")
	}
	return fail(lastErr)
}

// requiredContext estimates the context window a request needs: prompt tokens
// plus the requested output budget.
func requiredContext(req *canonical.CanonicalRequest) int {
	need := session.EstimateRequestTokens(req)
	if req.Params.MaxTokens != nil {
		need += *req.Params.MaxTokens
	}
	return need
}

func unixSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func (r *Router) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	return dispatch(r, ctx, req, func(p providers.Provider, q *canonical.CanonicalRequest, onUsage func(canonical.Usage)) (*canonical.CanonicalResponse, error) {
		resp, err := p.Execute(ctx, q)
		if err == nil && resp != nil {
			onUsage(resp.Usage)
		}
		return resp, err
	})
}

func (r *Router) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	return dispatch(r, ctx, req, func(p providers.Provider, q *canonical.CanonicalRequest, onUsage func(canonical.Usage)) (<-chan canonical.CanonicalEvent, error) {
		in, err := p.Stream(ctx, q)
		if err != nil {
			return nil, err
		}
		out := make(chan canonical.CanonicalEvent)
		go func() {
			defer close(out)
			var usage canonical.Usage
			seen := false
			for ev := range in {
				if ev.Usage != nil {
					usage = mergeUsage(usage, *ev.Usage)
					seen = true
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					for range in {
					}
					if seen {
						onUsage(usage)
					}
					return
				}
			}
			if seen {
				onUsage(usage)
			}
		}()
		return out, nil
	})
}

// mergeUsage keeps the largest value seen for each counter, since providers
// report cumulative usage across message_start / message_delta events.
func mergeUsage(a, b canonical.Usage) canonical.Usage {
	a.PromptTokens = max(a.PromptTokens, b.PromptTokens)
	a.CompletionTokens = max(a.CompletionTokens, b.CompletionTokens)
	a.TotalTokens = max(a.TotalTokens, b.TotalTokens)
	a.CacheReadInputTokens = max(a.CacheReadInputTokens, b.CacheReadInputTokens)
	a.CacheCreationInputTokens = max(a.CacheCreationInputTokens, b.CacheCreationInputTokens)
	a.ReasoningTokens = max(a.ReasoningTokens, b.ReasoningTokens)
	return a
}
