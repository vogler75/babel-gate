package smart

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
)

const stickyTTL = 30 * time.Minute

// Decision is the outcome of smart routing for one request.
type Decision struct {
	Tier             Tier
	Targets          []string // ordered "provider/model" candidates
	Reason           string
	Sticky           bool          // tier was reused from the session's current turn
	DecisionDuration time.Duration // time taken to make the decision
	FallbackUsed     bool          // true if primary classifier failed or was below minConfidence, falling back to heuristic
}

// Router classifies smart requests and keeps the tier stable for the rest of
// an agent turn, so tool-result follow-ups do not switch models mid-turn
// (which would discard the prompt cache and provider-signed reasoning).
type Router struct {
	tiers         map[Tier][]string
	primary       Classifier
	fallback      Classifier
	minConfidence float64
	sticky        bool

	mu       sync.Mutex
	sessions map[string]stickyEntry
}

type stickyEntry struct {
	tier Tier
	seen time.Time
}

// NewRouter builds a router from configuration. It returns nil when smart
// routing is not configured.
func NewRouter(cfg config.SmartConfig) (*Router, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	r := &Router{
		tiers:         make(map[Tier][]string),
		fallback:      Heuristic{},
		minConfidence: cfg.Classifier.MinConfidence,
		sticky:        !strings.EqualFold(cfg.Sticky, "none"),
		sessions:      make(map[string]stickyEntry),
	}
	if r.minConfidence <= 0 {
		r.minConfidence = 0.6
	}
	for name, targets := range cfg.Tiers {
		tier, _ := ParseTier(name)
		r.tiers[tier] = append([]string(nil), targets...)
	}
	switch strings.ToLower(cfg.Classifier.Mode) {
	case "", "heuristic":
		r.primary = Heuristic{}
	case "http":
		timeout := time.Duration(cfg.Classifier.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 400 * time.Millisecond
		}
		r.primary = NewHTTPClassifier(cfg.Classifier.URL, timeout)
	case "laya":
		timeout := time.Duration(cfg.Classifier.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = time.Second
		}
		r.primary = NewLayaClassifier(cfg.Classifier.URL, cfg.Classifier.APIKey, cfg.Classifier.Model, timeout)
	}
	return r, nil
}

// Validate checks a smart configuration without building it.
func Validate(cfg config.SmartConfig) error {
	for name, targets := range cfg.Tiers {
		if _, err := ParseTier(name); err != nil {
			return err
		}
		for _, target := range targets {
			if strings.TrimSpace(target) == "" {
				return fmt.Errorf("smart tier %q has an empty target", name)
			}
			if IsSmartModel(target) {
				return fmt.Errorf("smart tier %q cannot target the smart model itself", name)
			}
		}
	}
	switch strings.ToLower(cfg.Classifier.Mode) {
	case "", "heuristic":
	case "http", "laya":
		if cfg.Classifier.URL == "" {
			return fmt.Errorf("smart classifier mode %s requires a url", strings.ToLower(cfg.Classifier.Mode))
		}
	default:
		return fmt.Errorf("unknown smart classifier mode %q (expected heuristic, http or laya)", cfg.Classifier.Mode)
	}
	if s := strings.ToLower(cfg.Sticky); s != "" && s != "turn" && s != "none" {
		return fmt.Errorf("unknown smart sticky mode %q (expected turn or none)", cfg.Sticky)
	}
	return nil
}

// Decide picks the tier and ordered targets for a smart request.
func (r *Router) Decide(ctx context.Context, req *canonical.CanonicalRequest) Decision {
	start := time.Now()
	if r.sticky && req.SessionID != "" && !IsNewTurn(req) {
		if tier, ok := r.stickyTier(req.SessionID); ok {
			return Decision{
				Tier:             tier,
				Targets:          r.TargetsFor(tier),
				Reason:           "continuing turn",
				Sticky:           true,
				DecisionDuration: time.Since(start),
			}
		}
	}

	fallbackUsed := false
	c, err := r.primary.Classify(ctx, req)
	if err != nil || c.Confidence < r.minConfidence {
		if err != nil {
			log.Printf("[SMART] classifier failed, using heuristic: %v", err)
		}
		if _, isHeuristic := r.primary.(Heuristic); !isHeuristic {
			c, _ = r.fallback.Classify(ctx, req)
			fallbackUsed = true
		}
	}

	if r.sticky && req.SessionID != "" {
		r.remember(req.SessionID, c.Tier)
	}
	return Decision{
		Tier:             c.Tier,
		Targets:          r.TargetsFor(c.Tier),
		Reason:           c.Reason,
		DecisionDuration: time.Since(start),
		FallbackUsed:     fallbackUsed,
	}
}

// TargetsFor returns a tier's targets. An unconfigured tier borrows from the
// next stronger tier, then from the next weaker one, so a partial config
// never leaves a request without a target.
func (r *Router) TargetsFor(tier Tier) []string {
	for t := tier; t <= TierReasoning; t++ {
		if targets := r.tiers[t]; len(targets) > 0 {
			return append([]string(nil), targets...)
		}
	}
	for t := tier - 1; t >= TierSimple; t-- {
		if targets := r.tiers[t]; len(targets) > 0 {
			return append([]string(nil), targets...)
		}
	}
	return nil
}

func (r *Router) stickyTier(sessionID string) (Tier, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sessionID]
	if !ok || time.Since(entry.seen) > stickyTTL {
		return 0, false
	}
	entry.seen = time.Now()
	r.sessions[sessionID] = entry
	return entry.tier, true
}

func (r *Router) remember(sessionID string, tier Tier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.sessions[sessionID] = stickyEntry{tier: tier, seen: now}
	if len(r.sessions) > 1000 {
		for id, entry := range r.sessions {
			if now.Sub(entry.seen) > stickyTTL {
				delete(r.sessions, id)
			}
		}
	}
}
