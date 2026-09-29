package classifier

import (
	"context"
	"strings"
)

type KeywordRule struct {
	Keywords []string `yaml:"keywords" json:"keywords"`
	Tier     string   `yaml:"tier" json:"tier"`
}

// Keywords matches explicit phrases (e.g. "ultrathink") case-insensitively.
type Keywords struct {
	Rules []KeywordRule
}

func DefaultKeywordRules() []KeywordRule {
	return []KeywordRule{{
		Keywords: []string{"ultrathink", "think hard", "architecture review", "security review", "race condition"},
		Tier:     string(Reasoning),
	}}
}

func (k *Keywords) Name() string { return "keywords" }

func (k *Keywords) Classify(_ context.Context, ask string) (Result, bool) {
	lower := strings.ToLower(ask)
	for _, rule := range k.Rules {
		tier, ok := ParseTier(rule.Tier)
		if !ok {
			continue
		}
		for _, kw := range rule.Keywords {
			if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
				return Result{Tier: tier, Confidence: 1, Source: "keywords", Reason: kw}, true
			}
		}
	}
	return Result{}, false
}

// Heuristic scores the ask by length and signal words. It always answers.
type Heuristic struct{}

var (
	reasoningWords = []string{"architecture", "concurrency", "deadlock", "security", "vulnerab", "performance", "optimiz", "migrat", "distributed", "prove", "trade-off", "tradeoff", "design a system"}
	complexWords   = []string{"refactor", "debug", "multi-file", "across the", "why does", "why is", "root cause", "api design", "implement", "integrate", "investigate", "failing test", "stack trace"}
	simpleWords    = []string{"typo", "rename", "format", "lint", "hello", "hi ", "thanks", "what is", "one-line", "comment"}
)

func (h *Heuristic) Name() string { return "heuristic" }

func countHits(text string, words []string) int {
	n := 0
	for _, w := range words {
		if strings.Contains(text, w) {
			n++
		}
	}
	return n
}

func (h *Heuristic) Classify(_ context.Context, ask string) (Result, bool) {
	lower := strings.ToLower(ask)
	words := len(strings.Fields(ask))
	score := 0.0
	switch {
	case words > 400:
		score += 2
	case words > 120:
		score += 1.5
	case words > 30:
		score += 1
	case words <= 8:
		score -= 0.5
	}
	score += 1.5 * float64(countHits(lower, reasoningWords))
	score += 1.0 * float64(min(countHits(lower, complexWords), 2))
	score -= 1.0 * float64(countHits(lower, simpleWords))
	score += 0.5 * float64(strings.Count(ask, "```")/2)

	var tier Tier
	switch {
	case score >= 3.5:
		tier = Reasoning
	case score >= 2:
		tier = Complex
	case score >= 0.5:
		tier = Medium
	default:
		tier = Simple
	}
	return Result{Tier: tier, Confidence: 0.5, Source: "heuristic"}, true
}
