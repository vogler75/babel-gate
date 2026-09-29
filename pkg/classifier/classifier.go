// Package classifier decides which complexity tier a request needs.
package classifier

import (
	"context"
	"regexp"
	"strings"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

type Tier string

const (
	Simple    Tier = "SIMPLE"
	Medium    Tier = "MEDIUM"
	Complex   Tier = "COMPLEX"
	Reasoning Tier = "REASONING"
)

var Tiers = []Tier{Simple, Medium, Complex, Reasoning}

func ParseTier(s string) (Tier, bool) {
	t := Tier(strings.ToUpper(strings.TrimSpace(s)))
	for _, known := range Tiers {
		if t == known {
			return t, true
		}
	}
	return "", false
}

func (t Tier) Index() int {
	for i, known := range Tiers {
		if t == known {
			return i
		}
	}
	return -1
}

type Result struct {
	Tier       Tier    `json:"tier"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`
	Reason     string  `json:"reason,omitempty"`
}

// Classifier returns ok=false when it cannot judge the ask, so the next
// classifier in a Chain gets a turn.
type Classifier interface {
	Name() string
	Classify(ctx context.Context, ask string) (Result, bool)
}

type Chain struct {
	Classifiers []Classifier
	Default     Tier
	MinTier     Tier
}

func (c *Chain) Name() string { return "chain" }

func (c *Chain) Classify(ctx context.Context, ask string) (Result, bool) {
	res := Result{Tier: c.Default, Source: "default"}
	if res.Tier == "" {
		res.Tier = Complex
	}
	if strings.TrimSpace(ask) != "" {
		for _, cl := range c.Classifiers {
			if r, ok := cl.Classify(ctx, ask); ok {
				res = r
				break
			}
		}
	}
	if c.MinTier != "" && res.Tier.Index() < c.MinTier.Index() {
		res.Reason = strings.TrimSpace(res.Reason + " raised to min tier")
		res.Tier = c.MinTier
	}
	return res, true
}

var reminderRE = regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`)

// ExtractAsk returns the newest user turn that contains human-written text,
// ignoring tool results and Claude Code system reminders.
func ExtractAsk(req *canonical.CanonicalRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != canonical.RoleUser {
			continue
		}
		var parts []string
		for _, p := range m.Parts {
			if p.Type == canonical.PartText {
				parts = append(parts, p.Text)
			}
		}
		text := strings.TrimSpace(reminderRE.ReplaceAllString(strings.Join(parts, "\n"), ""))
		if text != "" {
			return text
		}
	}
	return ""
}
