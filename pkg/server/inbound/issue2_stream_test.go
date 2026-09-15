package inbound

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/providers/google"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/session"
)

type cancellationProvider struct {
	canceled chan struct{}
	once     sync.Once
}

type fallbackAttributionProvider struct {
	name string
	err  error
}

type scriptedStreamProvider struct {
	name   string
	events []canonical.CanonicalEvent
}

func (p *scriptedStreamProvider) Name() string     { return p.name }
func (p *scriptedStreamProvider) Type() string     { return "test" }
func (p *scriptedStreamProvider) Endpoint() string { return "http://" + p.name + ".test" }
func (p *scriptedStreamProvider) Execute(context.Context, *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	return nil, errors.New("not implemented")
}
func (p *scriptedStreamProvider) Stream(ctx context.Context, _ *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	ch := make(chan canonical.CanonicalEvent)
	go func() {
		defer close(ch)
		for _, ev := range p.events {
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}
	}()
	return ch, nil
}
func (p *scriptedStreamProvider) ListModels(context.Context) ([]providers.ModelInfo, error) {
	return []providers.ModelInfo{{ID: "test-model", Provider: p.name}}, nil
}

func (p *fallbackAttributionProvider) Name() string     { return p.name }
func (p *fallbackAttributionProvider) Type() string     { return "test" }
func (p *fallbackAttributionProvider) Endpoint() string { return "http://" + p.name + ".test" }
func (p *fallbackAttributionProvider) Execute(context.Context, *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &canonical.CanonicalResponse{Model: "target", Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "ok"}}}, FinishReason: "stop"}, nil
}
func (p *fallbackAttributionProvider) Stream(context.Context, *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	return nil, p.err
}
func (p *fallbackAttributionProvider) ListModels(context.Context) ([]providers.ModelInfo, error) {
	return nil, nil
}

func (p *cancellationProvider) Name() string     { return "google" }
func (p *cancellationProvider) Type() string     { return "google" }
func (p *cancellationProvider) Endpoint() string { return "http://upstream.test" }
func (p *cancellationProvider) Execute(context.Context, *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	return nil, errors.New("not implemented")
}
func (p *cancellationProvider) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	ch := make(chan canonical.CanonicalEvent)
	go func() {
		defer close(ch)
		select {
		case ch <- canonical.CanonicalEvent{Type: canonical.EventTextDelta, Text: "token"}:
		case <-ctx.Done():
			p.once.Do(func() { close(p.canceled) })
			return
		}
		<-ctx.Done()
		p.once.Do(func() { close(p.canceled) })
	}()
	return ch, nil
}
func (p *cancellationProvider) ListModels(context.Context) ([]providers.ModelInfo, error) {
	return []providers.ModelInfo{{ID: "gemini-test", Provider: "google"}}, nil
}

type failingStreamWriter struct {
	header   http.Header
	body     strings.Builder
	writes   int
	failAt   int
	flushErr error
}

func (w *failingStreamWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *failingStreamWriter) WriteHeader(int) {}
func (w *failingStreamWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failAt > 0 && w.writes >= w.failAt {
		return 0, errors.New("client socket closed")
	}
	return w.body.Write(p)
}
func (w *failingStreamWriter) FlushError() error { return w.flushErr }

func TestIssue2AnthropicDownstreamFailureCancelsAndRecordsError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failAt   int
		flushErr error
	}{
		{name: "write", failAt: 2},
		{name: "flush", flushErr: errors.New("flush failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := router.NewEngine(&config.Config{Providers: map[string]config.ProviderConfig{}, Routing: config.RoutingConfig{Routes: map[string]string{}, Fallbacks: map[string][]string{}}})
			if err != nil {
				t.Fatal(err)
			}
			provider := &cancellationProvider{canceled: make(chan struct{})}
			engine.RegisterProvider(provider)
			sessions := session.NewManager()
			sess := sessions.GetOrCreate("issue2", "127.0.0.1", "test", "test")
			handler := NewAnthropicHandler(engine, router.NewCatalog(engine), sessions)
			writer := &failingStreamWriter{failAt: tc.failAt, flushErr: tc.flushErr}
			req, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			httpReq, err := http.NewRequestWithContext(req, http.MethodPost, "http://router.test/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			handler.handleStreaming(writer, httpReq, &canonical.CanonicalRequest{
				Model: "gemini-test", SessionID: sess.ID,
				Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "hi"}}}},
			}, sess, time.Now())

			select {
			case <-provider.canceled:
			case <-time.After(time.Second):
				t.Fatal("upstream context was not canceled after downstream failure")
			}
			if strings.Contains(writer.body.String(), "message_stop") {
				t.Fatalf("downstream failure emitted false success terminator: %s", writer.body.String())
			}
			got := sessions.ListSessions()[0].RecentRequests
			if len(got) != 1 || got[0].Status != "error" || got[0].ErrorMessage == "" {
				t.Fatalf("failure telemetry was not finalized: %+v", got)
			}
		})
	}
}

func TestIssue2SSEMarshalFailureIsReturned(t *testing.T) {
	writer := &failingStreamWriter{}
	err := newSSEWriter(writer).data(map[string]any{"bad": func() {}})
	if err == nil || !strings.Contains(err.Error(), "marshal SSE data") || writer.writes != 0 {
		t.Fatalf("marshal failure was not returned before writing: err=%v writes=%d", err, writer.writes)
	}
}

func TestIssue2AnthropicUpstreamErrorAndCancellationFinalizeAsFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		events    []canonical.CanonicalEvent
		cancelReq bool
	}{
		{name: "upstream error", events: []canonical.CanonicalEvent{{Type: canonical.EventError, Error: errors.New("upstream failed")}}},
		{name: "cancellation", cancelReq: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := router.NewEngine(&config.Config{Providers: map[string]config.ProviderConfig{}, Routing: config.RoutingConfig{Routes: map[string]string{}, Fallbacks: map[string][]string{}}})
			if err != nil {
				t.Fatal(err)
			}
			engine.RegisterProvider(&scriptedStreamProvider{name: "test", events: tc.events})
			sessions := session.NewManager()
			sess := sessions.GetOrCreate("issue2-"+tc.name, "127.0.0.1", "test", "test")
			handler := NewAnthropicHandler(engine, router.NewCatalog(engine), sessions)
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancelReq {
				cancel()
			} else {
				defer cancel()
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
			recorder := httptest.NewRecorder()
			handler.handleStreaming(recorder, req, &canonical.CanonicalRequest{Model: "test-model", SessionID: sess.ID}, sess, time.Now())

			if strings.Contains(recorder.Body.String(), "message_stop") {
				t.Fatalf("%s emitted false success framing: %s", tc.name, recorder.Body.String())
			}
			requests := sessions.ListSessions()[0].RecentRequests
			if len(requests) != 1 || requests[0].Status != "error" || requests[0].ErrorMessage == "" {
				t.Fatalf("%s telemetry was not finalized as a failure: %+v", tc.name, requests)
			}
		})
	}
}

func TestIssue2NilGeminiArgumentsCompleteAnthropicToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data:{\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"tool\"}}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer upstream.Close()

	engine, err := router.NewEngine(&config.Config{Providers: map[string]config.ProviderConfig{}, Routing: config.RoutingConfig{Routes: map[string]string{}, Fallbacks: map[string][]string{}}})
	if err != nil {
		t.Fatal(err)
	}
	engine.RegisterProvider(google.NewClientWithTimeouts("google", "test", upstream.URL, nil, upstream.Client(), google.Timeouts{ResponseHeader: time.Second, StreamIdle: time.Second}))
	handler := NewAnthropicHandler(engine, router.NewCatalog(engine), nil)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler.handleStreaming(recorder, req, &canonical.CanonicalRequest{Model: "gemini-test"}, nil, time.Now())
	body := recorder.Body.String()
	for _, want := range []string{`"partial_json":"{}"`, `"stop_reason":"tool_use"`, `event: message_stop`} {
		if !strings.Contains(body, want) {
			t.Fatalf("Anthropic tool-use stream is incomplete; missing %q in %s", want, body)
		}
	}
}

func TestIssue2SuccessfulFallbackUsesExecutedRouteInTelemetry(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Routing: config.RoutingConfig{
			Routes:    map[string]string{"requested": "primary/primary-model"},
			Fallbacks: map[string][]string{"requested": {"secondary/fallback-model"}},
		},
	}
	engine, err := router.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine.RegisterProvider(&fallbackAttributionProvider{name: "primary", err: errors.New("primary failed")})
	engine.RegisterProvider(&fallbackAttributionProvider{name: "secondary"})
	sessions := session.NewManager()
	sess := sessions.GetOrCreate("fallback", "127.0.0.1", "test", "test")
	handler := NewAnthropicHandler(engine, router.NewCatalog(engine), sessions)
	tr := trace.New(http.MethodPost, "/v1/messages")
	tr.SetRoute("requested", "primary", "http://primary.test", "primary-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(trace.WithTrace(context.Background(), tr))
	recorder := httptest.NewRecorder()
	handler.handleNonStreaming(recorder, req, &canonical.CanonicalRequest{
		Model: "requested", SessionID: sess.ID,
		Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "hi"}}}},
	}, sess, time.Now())

	requests := sessions.ListSessions()[0].RecentRequests
	if len(requests) != 1 || requests[0].Provider != "secondary" || requests[0].Model != "secondary/fallback-model" || requests[0].Status != "success" {
		t.Fatalf("fallback telemetry used the wrong route: %+v", requests)
	}
}
