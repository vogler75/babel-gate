package budget

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
)

func copilotBudget() map[string]config.BudgetConfig {
	return map[string]config.BudgetConfig{
		"copilot": {
			Limit:        10,
			PeriodDays:   30,
			TierWeights:  map[string]float64{"REASONING": 0.6, "COMPLEX": 0.3, "MEDIUM": 0.07, "SIMPLE": 0.03},
			Prices:       map[string][]float64{"claude-opus-5.5": {4, 20, 0.2, 5}},
			DefaultPrice: []float64{3, 15, 0.3, 3.75},
		},
	}
}

func TestCost(t *testing.T) {
	tr := New(copilotBudget())
	u := canonical.Usage{PromptTokens: 1_000_000, CompletionTokens: 100_000, CacheReadInputTokens: 500_000}
	got := tr.Cost("copilot", "claude-opus-5.5", u)
	want := 0.5*4 + 0.1*20 + 0.5*0.2
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
	if tr.Cost("sdc", "x", u) != 0 {
		t.Fatal("unbudgeted provider should cost 0")
	}
}

func TestTierSharesAndWindow(t *testing.T) {
	tr := New(copilotBudget())
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return now }

	if !tr.Allowed("copilot", "SIMPLE") || !tr.Allowed("sdc", "SIMPLE") {
		t.Fatal("fresh budget should allow")
	}
	tr.Record("copilot", "other", "SIMPLE", canonical.Usage{PromptTokens: 100_000})
	if tr.Allowed("copilot", "SIMPLE") {
		t.Fatal("SIMPLE share (0.30) should be exhausted by 0.30 spend")
	}
	if !tr.Allowed("copilot", "REASONING") {
		t.Fatal("REASONING share is independent")
	}
	now = now.AddDate(0, 0, 30)
	if !tr.Allowed("copilot", "SIMPLE") {
		t.Fatal("spend should roll out of the 30-day window")
	}
}

func TestUnweightedAndUnknownTier(t *testing.T) {
	tr := New(map[string]config.BudgetConfig{"sdc": {Limit: 1, DefaultPrice: []float64{1, 1}}})
	tr.Record("sdc", "m", "SIMPLE", canonical.Usage{PromptTokens: 600_000})
	if !tr.Allowed("sdc", "REASONING") {
		t.Fatal("0.6 of 1 spent: still allowed")
	}
	tr.Record("sdc", "m", "COMPLEX", canonical.Usage{PromptTokens: 600_000})
	if tr.Allowed("sdc", "SIMPLE") {
		t.Fatal("unweighted budget is shared across tiers")
	}
	w := New(map[string]config.BudgetConfig{"c": {Limit: 1, TierWeights: map[string]float64{"COMPLEX": 1}}})
	if w.Allowed("c", "SIMPLE") {
		t.Fatal("tier without weight has no share")
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	tr, err := Open(path, copilotBudget())
	if err != nil {
		t.Fatal(err)
	}
	tr.Record("copilot", "other", "SIMPLE", canonical.Usage{PromptTokens: 100_000})
	_ = tr.Close()

	tr2, err := Open(path, copilotBudget())
	if err != nil {
		t.Fatal(err)
	}
	defer tr2.Close()
	if tr2.Allowed("copilot", "SIMPLE") {
		t.Fatal("spend should survive restart")
	}
	st := tr2.Status()
	if len(st) != 4 {
		t.Fatalf("status = %+v", st)
	}
}
