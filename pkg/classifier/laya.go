package classifier

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

var layaQuestions = map[string]any{
	"tier": map[string]any{
		"type":         "choice",
		"instructions": "How much model capability does this software engineering request need?",
		"criteria": map[string]string{
			"SIMPLE":    "trivial: typo, rename, formatting, one-line fix, greeting, short factual question",
			"MEDIUM":    "routine: small feature, single-file change, write a test, explain code",
			"COMPLEX":   "hard: multi-file change, refactor, debugging with unclear cause, API design",
			"REASONING": "very hard: architecture, concurrency, security, performance analysis, large migration",
		},
	},
}

// Laya asks a laya-serve instance (POST /v1/systemone) for the tier. Errors,
// timeouts, unknown choices and low confidence all return ok=false.
type Laya struct {
	URL      string
	APIKey   string
	Model    string
	MinProb  float64
	MaxChars int
	Client   *http.Client

	cache *lruCache
}

func NewLaya(baseURL, apiKey, model string, timeout time.Duration, minProb float64, maxChars, cacheSize int) *Laya {
	if timeout <= 0 {
		timeout = 800 * time.Millisecond
	}
	if minProb <= 0 {
		minProb = 0.40
	}
	if maxChars <= 0 {
		maxChars = 1500
	}
	if cacheSize <= 0 {
		cacheSize = 512
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "auto" {
		model = ""
	}
	return &Laya{
		URL:      strings.TrimSuffix(baseURL, "/") + "/v1/systemone",
		APIKey:   apiKey,
		Model:    model,
		MinProb:  minProb,
		MaxChars: maxChars,
		Client:   &http.Client{Timeout: timeout},
		cache:    newLRU(cacheSize),
	}
}

func (l *Laya) Name() string { return "laya" }

type layaAnswer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
}

func (l *Laya) Classify(ctx context.Context, ask string) (Result, bool) {
	if len(ask) > l.MaxChars {
		ask = ask[:l.MaxChars]
	}
	sum := sha256.Sum256([]byte(ask))
	key := hex.EncodeToString(sum[:])
	if r, ok := l.cache.get(key); ok {
		return r, r.Tier != ""
	}

	body := map[string]any{"state": map[string]string{"request": ask}, "questions": layaQuestions}
	if l.Model != "" {
		body["model"] = l.Model
	}
	start := time.Now()
	answer, err := l.post(ctx, body)
	if err != nil {
		log.Printf("[SMART] laya unavailable (%v), falling back", err)
		return Result{}, false
	}
	choice := strings.ToUpper(answer.Choice)
	prob := answer.Probabilities[answer.Choice]
	if prob == 0 {
		prob = answer.Probabilities[choice]
	}
	tier, known := ParseTier(choice)
	res := Result{Confidence: prob, Source: "laya", Reason: fmt.Sprintf("%s p=%.2f %dms", choice, prob, time.Since(start).Milliseconds())}
	switch {
	case !known:
		res.Reason = "unknown choice " + choice
	case prob < l.MinProb:
		res.Reason = fmt.Sprintf("low confidence %s p=%.2f", choice, prob)
	default:
		res.Tier = tier
	}
	l.cache.put(key, res)
	return res, res.Tier != ""
}

func (l *Laya) post(ctx context.Context, body any) (*layaAnswer, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.APIKey != "" && !strings.HasPrefix(l.APIKey, "${") {
		req.Header.Set("Authorization", "Bearer "+l.APIKey)
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var data struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	raw, ok := data.Answers["tier"]
	if !ok {
		return nil, fmt.Errorf("answers.tier missing")
	}
	var answer layaAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("answers.tier: %w", err)
	}
	return &answer, nil
}

// HTTP calls a generic classifier endpoint: POST {"ask": "..."} and expects
// {"tier": "COMPLEX", "confidence": 0.8}.
type HTTP struct {
	URL     string
	APIKey  string
	MinProb float64
	Client  *http.Client
}

func NewHTTP(url, apiKey string, timeout time.Duration, minProb float64) *HTTP {
	if timeout <= 0 {
		timeout = 800 * time.Millisecond
	}
	return &HTTP{URL: url, APIKey: apiKey, MinProb: minProb, Client: &http.Client{Timeout: timeout}}
}

func (h *HTTP) Name() string { return "http" }

func (h *HTTP) Classify(ctx context.Context, ask string) (Result, bool) {
	payload, _ := json.Marshal(map[string]string{"ask": ask})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(payload))
	if err != nil {
		return Result{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	if h.APIKey != "" && !strings.HasPrefix(h.APIKey, "${") {
		req.Header.Set("Authorization", "Bearer "+h.APIKey)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		log.Printf("[SMART] http classifier unavailable (%v), falling back", err)
		return Result{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return Result{}, false
	}
	var data struct {
		Tier       string  `json:"tier"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return Result{}, false
	}
	tier, ok := ParseTier(data.Tier)
	if !ok || data.Confidence < h.MinProb {
		return Result{}, false
	}
	return Result{Tier: tier, Confidence: data.Confidence, Source: "http"}, true
}

type lruCache struct {
	mu    sync.Mutex
	size  int
	order *list.List
	items map[string]*list.Element
}

type lruEntry struct {
	key string
	val Result
}

func newLRU(size int) *lruCache {
	return &lruCache{size: size, order: list.New(), items: make(map[string]*list.Element)}
}

func (c *lruCache) get(key string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*lruEntry).val, true
	}
	return Result{}, false
}

func (c *lruCache) put(key string, val Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*lruEntry).val = val
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&lruEntry{key: key, val: val})
	for c.order.Len() > c.size {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*lruEntry).key)
	}
}
