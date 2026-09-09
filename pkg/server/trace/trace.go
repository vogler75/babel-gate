package trace

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}

// RequestTrace records fine-grained latency and routing details for an HTTP request.
type RequestTrace struct {
	mu sync.RWMutex

	StartTime time.Time
	Method    string
	Path      string

	// Routing details
	RequestedModel string
	Provider       string
	Destination    string // e.g. "https://llm.sdc.siemens.cloud/v1" or "https://api.githubcopilot.com"
	TargetModel    string

	// Token usage
	InputTokens  int
	OutputTokens int
	IsStream     bool

	// Latency stages
	ReadDuration     time.Duration // Time to read request body from client
	TTFT             time.Duration // Time-to-first-token from request start
	StreamDuration   time.Duration // Time streaming tokens (from first token to EOF)
	UpstreamDuration time.Duration // Total upstream call duration (for non-streaming)

	firstTokenAt time.Time
	notes        []string
	done         bool
}

// New creates a new RequestTrace initialized with the request method and path.
func New(method, path string) *RequestTrace {
	return &RequestTrace{
		StartTime: time.Now(),
		Method:    method,
		Path:      path,
	}
}

// WithTrace returns a new context carrying the RequestTrace.
func WithTrace(ctx context.Context, tr *RequestTrace) context.Context {
	return context.WithValue(ctx, contextKey{}, tr)
}

// FromContext retrieves the RequestTrace from the context, or nil if not present.
func FromContext(ctx context.Context) *RequestTrace {
	if ctx == nil {
		return nil
	}
	tr, _ := ctx.Value(contextKey{}).(*RequestTrace)
	return tr
}

// SetRoute records the model and provider destination info.
func (t *RequestTrace) SetRoute(reqModel, provider, destination, targetModel string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.RequestedModel = reqModel
	t.Provider = provider
	t.Destination = destination
	t.TargetModel = targetModel
}

// SetReadDuration records the time spent reading the request body from the client.
func (t *RequestTrace) SetReadDuration(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ReadDuration = d
}

// MarkFirstToken marks the arrival of the first token/chunk from the upstream provider.
func (t *RequestTrace) MarkFirstToken() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.firstTokenAt.IsZero() {
		t.firstTokenAt = time.Now()
		t.TTFT = t.firstTokenAt.Sub(t.StartTime)
		t.IsStream = true
	}
}

// MarkStreamDone records completion of the streaming phase.
func (t *RequestTrace) MarkStreamDone() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.firstTokenAt.IsZero() && t.StreamDuration == 0 {
		t.StreamDuration = time.Since(t.firstTokenAt)
	}
	t.done = true
}

// GenerationDuration returns the measured token-generation interval for a
// streaming response, excluding time-to-first-token.
func (t *RequestTrace) GenerationDuration() time.Duration {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.StreamDuration
}

// SetUpstreamDuration records the duration of a non-streaming upstream execution.
func (t *RequestTrace) SetUpstreamDuration(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.UpstreamDuration = d
	t.done = true
}

// SetTokens records input and output token counts.
func (t *RequestTrace) SetTokens(inTokens, outTokens int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if inTokens > 0 {
		t.InputTokens = inTokens
	}
	if outTokens > 0 {
		t.OutputTokens = outTokens
	}
}

// AddNote adds an informational note (e.g. fallback provider attempts).
func (t *RequestTrace) AddNote(note string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notes = append(t.notes, note)
}

// HasFirstToken reports whether the first token has been received.
func (t *RequestTrace) HasFirstToken() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return !t.firstTokenAt.IsZero()
}

// IsDone reports whether the request handling has completed.
func (t *RequestTrace) IsDone() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.done
}

// ProgressInfo returns current in-flight progress information.
func (t *RequestTrace) ProgressInfo() (modelDesc string, destination string, hasFirstToken bool, elapsed time.Duration, outTokens int) {
	if t == nil {
		return "", "", false, 0, 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	elapsed = time.Since(t.StartTime)
	hasFirstToken = !t.firstTokenAt.IsZero()
	outTokens = t.OutputTokens
	destination = t.Destination

	model := t.RequestedModel
	if model == "" {
		model = t.TargetModel
	}
	if t.Provider != "" && t.Provider != "unknown" {
		modelDesc = fmt.Sprintf("%s via %s", model, t.Provider)
	} else if model != "" {
		modelDesc = model
	}

	return modelDesc, destination, hasFirstToken, elapsed, outTokens
}

// FormatDuration formats a duration in a concise, readable manner.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	mins := int(d.Minutes())
	secs := math.Mod(d.Seconds(), 60)
	return fmt.Sprintf("%dm%.1fs", mins, secs)
}

func formatTokenCount(n int) string {
	if n >= 100000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000.0)
	}
	if n >= 10000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000.0)
	}
	if n >= 1000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return fmt.Sprintf("%d", n)
}

// FormatRoute returns the model, provider, destination, and token usage summary, e.g.:
// [claude-3-7-sonnet via copilot -> https://api.githubcopilot.com: 12.5k in / 850 out]
func (t *RequestTrace) FormatRoute() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	model := t.RequestedModel
	if model == "" {
		model = t.TargetModel
	}
	if model == "" {
		return ""
	}

	var parts []string
	routeDesc := model
	if t.Provider != "" && t.Provider != "unknown" {
		routeDesc = fmt.Sprintf("%s via %s", model, t.Provider)
	}
	if t.Destination != "" {
		routeDesc = fmt.Sprintf("%s -> %s", routeDesc, t.Destination)
	}
	parts = append(parts, routeDesc)

	if t.InputTokens > 0 || t.OutputTokens > 0 {
		tokenDesc := fmt.Sprintf("%s in / %s out", formatTokenCount(t.InputTokens), formatTokenCount(t.OutputTokens))
		parts = append(parts, tokenDesc)
	}

	return fmt.Sprintf(" [%s]", strings.Join(parts, ": "))
}

// FormatBreakdown returns the timing breakdown, e.g.:
// (read: 15ms, ttft: 4.20s, stream: 52.10s [16.3 tok/s])
func (t *RequestTrace) FormatBreakdown() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	var stages []string

	// Read duration (client payload upload/decode)
	if t.ReadDuration >= time.Millisecond {
		stages = append(stages, fmt.Sprintf("read: %s", FormatDuration(t.ReadDuration)))
	}

	// Notes (e.g. fallback attempts)
	for _, n := range t.notes {
		stages = append(stages, n)
	}

	// Streaming latency
	if t.IsStream || t.TTFT > 0 {
		if t.TTFT > 0 {
			stages = append(stages, fmt.Sprintf("ttft: %s", FormatDuration(t.TTFT)))
		}
		if t.StreamDuration > 0 {
			streamDesc := fmt.Sprintf("stream: %s", FormatDuration(t.StreamDuration))
			if t.OutputTokens > 0 && t.StreamDuration.Seconds() > 0 {
				tokPerSec := float64(t.OutputTokens) / t.StreamDuration.Seconds()
				streamDesc += fmt.Sprintf(" [%.1f tok/s]", tokPerSec)
			}
			stages = append(stages, streamDesc)
		}
	} else if t.UpstreamDuration > 0 {
		stages = append(stages, fmt.Sprintf("upstream: %s", FormatDuration(t.UpstreamDuration)))
	}

	if len(stages) == 0 {
		return ""
	}

	return fmt.Sprintf(" (%s)", strings.Join(stages, ", "))
}
