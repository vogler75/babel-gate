// Package budget tracks estimated provider spend over a rolling window and
// reports whether a provider/tier combination still has budget left.
package budget

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"

	_ "modernc.org/sqlite"
)

const dayLayout = "2006-01-02"

type key struct {
	provider string
	tier     string
	day      string
}

// Tracker keeps daily spend per provider and tier in memory and, when opened
// with a database path, persists it to SQLite so restarts keep the window.
type Tracker struct {
	mu      sync.Mutex
	db      *sql.DB
	budgets map[string]config.BudgetConfig
	spend   map[key]float64
	now     func() time.Time
}

// New returns an in-memory tracker.
func New(budgets map[string]config.BudgetConfig) *Tracker {
	return &Tracker{budgets: budgets, spend: make(map[key]float64), now: time.Now}
}

// Open returns a tracker persisted in the SQLite database at path.
func Open(path string, budgets map[string]config.BudgetConfig) (*Tracker, error) {
	t := New(budgets)
	if path == "" {
		return t, nil
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating budget db directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening budget db: %w", err)
	}
	for _, stmt := range []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		`CREATE TABLE IF NOT EXISTS budget_spend (
			day TEXT NOT NULL,
			provider TEXT NOT NULL,
			tier TEXT NOT NULL,
			model TEXT NOT NULL,
			requests INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (day, provider, tier, model)
		);`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initializing budget db: %w", err)
		}
	}
	cutoff := t.now().UTC().AddDate(0, 0, -t.maxPeriod()).Format(dayLayout)
	rows, err := db.Query(`SELECT day, provider, tier, SUM(cost) FROM budget_spend WHERE day >= ? GROUP BY day, provider, tier`, cutoff)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("loading budget spend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k key
		var cost float64
		if err := rows.Scan(&k.day, &k.provider, &k.tier, &cost); err != nil {
			_ = db.Close()
			return nil, err
		}
		t.spend[k] = cost
	}
	if err := rows.Err(); err != nil {
		_ = db.Close()
		return nil, err
	}
	t.db = db
	return t, nil
}

func (t *Tracker) Close() error {
	if t == nil || t.db == nil {
		return nil
	}
	return t.db.Close()
}

func (t *Tracker) maxPeriod() int {
	days := 30
	for _, b := range t.budgets {
		if b.PeriodDays > days {
			days = b.PeriodDays
		}
	}
	return days
}

func period(b config.BudgetConfig) int {
	if b.PeriodDays <= 0 {
		return 30
	}
	return b.PeriodDays
}

// SetBudgets replaces the budget configuration; recorded spend is kept.
func (t *Tracker) SetBudgets(budgets map[string]config.BudgetConfig) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.budgets = budgets
	t.mu.Unlock()
}

// Cost estimates the price of one request from its token usage.
func (t *Tracker) Cost(provider, model string, u canonical.Usage) float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.costLocked(provider, model, u)
}

func (t *Tracker) costLocked(provider, model string, u canonical.Usage) float64 {
	b, ok := t.budgets[provider]
	if !ok {
		return 0
	}
	price, ok := b.Prices[model]
	if !ok {
		price = b.DefaultPrice
	}
	if len(price) == 0 {
		return 0
	}
	p := func(i int) float64 {
		if i < len(price) {
			return price[i]
		}
		return 0
	}
	fresh := u.PromptTokens - u.CacheReadInputTokens - u.CacheCreationInputTokens
	if fresh < 0 {
		fresh = 0
	}
	return (float64(fresh)*p(0) + float64(u.CompletionTokens)*p(1) +
		float64(u.CacheReadInputTokens)*p(2) + float64(u.CacheCreationInputTokens)*p(3)) / 1e6
}

// limitFor returns the cap for provider/tier and whether spend is counted
// per tier (weighted) or across the whole provider.
func limitFor(b config.BudgetConfig, tier string) (limit float64, perTier bool) {
	if len(b.TierWeights) == 0 {
		return b.Limit, false
	}
	return b.Limit * b.TierWeights[strings.ToUpper(tier)], true
}

func (t *Tracker) spent(provider, tier string, perTier bool, days int) float64 {
	cutoff := t.now().UTC().AddDate(0, 0, -(days - 1)).Format(dayLayout)
	var total float64
	for k, v := range t.spend {
		if k.provider != provider || k.day < cutoff {
			continue
		}
		if perTier && k.tier != tier {
			continue
		}
		total += v
	}
	return total
}

// Allowed reports whether provider still has budget for tier. Providers
// without a configured budget, or with a non-positive limit, are unlimited.
func (t *Tracker) Allowed(provider, tier string) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.budgets[provider]
	if !ok || b.Limit <= 0 {
		return true
	}
	limit, perTier := limitFor(b, tier)
	if limit <= 0 {
		return false
	}
	return t.spent(provider, strings.ToUpper(tier), perTier, period(b)) < limit
}

// Record adds the estimated cost of a completed request and returns it.
func (t *Tracker) Record(provider, model, tier string, u canonical.Usage) float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	if _, ok := t.budgets[provider]; !ok {
		t.mu.Unlock()
		return 0
	}
	cost := t.costLocked(provider, model, u)
	k := key{provider: provider, tier: strings.ToUpper(tier), day: t.now().UTC().Format(dayLayout)}
	t.spend[k] += cost
	t.mu.Unlock()
	if t.db != nil {
		_, _ = t.db.Exec(`INSERT INTO budget_spend (day, provider, tier, model, requests, cost) VALUES (?, ?, ?, ?, 1, ?)
			ON CONFLICT(day, provider, tier, model) DO UPDATE SET requests = requests + 1, cost = cost + excluded.cost`,
			k.day, provider, k.tier, model, cost)
	}
	return cost
}

// Status describes the budget state of one provider/tier pair.
type Status struct {
	Provider   string  `json:"provider"`
	Tier       string  `json:"tier,omitempty"`
	Spent      float64 `json:"spent"`
	Limit      float64 `json:"limit"`
	Currency   string  `json:"currency"`
	PeriodDays int     `json:"period_days"`
	Exhausted  bool    `json:"exhausted"`
}

// Status returns the current spend against every configured budget.
func (t *Tracker) Status() []Status {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Status
	for name, b := range t.budgets {
		days := period(b)
		if len(b.TierWeights) == 0 {
			spent := t.spent(name, "", false, days)
			out = append(out, Status{Provider: name, Spent: spent, Limit: b.Limit, Currency: b.Currency, PeriodDays: days, Exhausted: b.Limit > 0 && spent >= b.Limit})
			continue
		}
		for tier := range b.TierWeights {
			tier = strings.ToUpper(tier)
			limit, _ := limitFor(b, tier)
			spent := t.spent(name, tier, true, days)
			out = append(out, Status{Provider: name, Tier: tier, Spent: spent, Limit: limit, Currency: b.Currency, PeriodDays: days, Exhausted: spent >= limit})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Tier < out[j].Tier
	})
	return out
}
