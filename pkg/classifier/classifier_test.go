package classifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func TestExtractAskSkipsToolResultsAndReminders(t *testing.T) {
	req := &canonical.CanonicalRequest{Messages: []canonical.Message{
		{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "Refactor the router"}}},
		{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{{Type: canonical.PartToolCall, ToolCallName: "Read"}}},
		{Role: canonical.RoleUser, Parts: []canonical.ContentPart{
			{Type: canonical.PartToolResult, ToolResultContent: "file body"},
			{Type: canonical.PartText, Text: "<system-reminder>ignore me</system-reminder>"},
		}},
	}}
	if got := ExtractAsk(req); got != "Refactor the router" {
		t.Fatalf("ExtractAsk = %q", got)
	}
}

func TestKeywordsAndHeuristic(t *testing.T) {
	ctx := context.Background()
	kw := &Keywords{Rules: DefaultKeywordRules()}
	if r, ok := kw.Classify(ctx, "please ULTRATHINK about this"); !ok || r.Tier != Reasoning {
		t.Fatalf("keyword result = %+v %v", r, ok)
	}
	if _, ok := kw.Classify(ctx, "fix typo"); ok {
		t.Fatal("unexpected keyword match")
	}

	h := &Heuristic{}
	cases := map[string]Tier{
		"fix the typo in README": Simple,
		"Refactor the session manager and debug why the failing test times out across the packages":                    Complex,
		"Review the architecture for concurrency and security issues, then plan the migration to a distributed design": Reasoning,
	}
	for ask, want := range cases {
		if r, _ := h.Classify(ctx, ask); r.Tier != want {
			t.Errorf("heuristic(%q) = %s, want %s", ask, r.Tier, want)
		}
	}
}

func TestChainMinTierAndDefault(t *testing.T) {
	ctx := context.Background()
	c := &Chain{Classifiers: []Classifier{&Heuristic{}}, MinTier: Medium}
	if r, _ := c.Classify(ctx, "hi"); r.Tier != Medium {
		t.Fatalf("min tier not applied: %+v", r)
	}
	if r, _ := c.Classify(ctx, ""); r.Tier != Complex || r.Source != "default" {
		t.Fatalf("empty ask should use default: %+v", r)
	}
}

func layaServer(t *testing.T, choice string, prob float64, calls *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["questions"].(map[string]any)["tier"]; !ok {
			http.Error(w, "no tier question", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"tier": map[string]any{
			"choice": choice, "probabilities": map[string]float64{choice: prob},
		}}})
	}))
}

func TestLayaAcceptsConfidentAnswerAndCaches(t *testing.T) {
	var calls int32
	srv := layaServer(t, "COMPLEX", 0.8, &calls)
	defer srv.Close()
	l := NewLaya(srv.URL, "k", "auto", time.Second, 0.4, 0, 0)
	for i := 0; i < 2; i++ {
		r, ok := l.Classify(context.Background(), "refactor everything")
		if !ok || r.Tier != Complex || r.Source != "laya" {
			t.Fatalf("laya result = %+v %v", r, ok)
		}
	}
	if calls != 1 {
		t.Fatalf("expected cached second call, got %d calls", calls)
	}
}

func TestLayaLowConfidenceAndOutageFallBack(t *testing.T) {
	var calls int32
	srv := layaServer(t, "SIMPLE", 0.2, &calls)
	l := NewLaya(srv.URL, "k", "", time.Second, 0.4, 0, 0)
	if _, ok := l.Classify(context.Background(), "x"); ok {
		t.Fatal("low confidence should not be accepted")
	}
	srv.Close()
	down := NewLaya(srv.URL, "k", "", 200*time.Millisecond, 0.4, 0, 0)
	c := &Chain{Classifiers: []Classifier{down, &Heuristic{}}}
	if r, _ := c.Classify(context.Background(), "fix typo"); r.Source != "heuristic" {
		t.Fatalf("outage should fall back to heuristic: %+v", r)
	}
}

func TestLRUEviction(t *testing.T) {
	c := newLRU(2)
	c.put("a", Result{Tier: Simple})
	c.put("b", Result{Tier: Medium})
	c.get("a")
	c.put("c", Result{Tier: Complex})
	if _, ok := c.get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a should remain")
	}
}
