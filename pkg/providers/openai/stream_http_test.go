package openai_test

import (
	"context"
	"fmt"
	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/providers/copilot"
	"github.com/vogler75/babel-gate/pkg/providers/openai"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProviderStreamErrors(t *testing.T) {
	for _, kind := range []string{"openai", "copilot"} {
		for _, tt := range []struct {
			name, data string
			wantError  bool
		}{
			{"first error", `data: {"error":{"message":"quota exhausted"}}` + "\n\n", true},
			{"partial error", `data:{"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n" + `data: {"error":{"message":"failed"}}` + "\n\n", true},
			{"malformed", "data: {oops\n\n", true},
			{"truncated", `data:{"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n", true},
			{"usage", `data:{"choices":[],"usage":{"prompt_tokens":1}}` + "\n\ndata:[DONE]\n\n", false},
			{"finish without sentinel", `data:{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n", false},
		} {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, tt.data)
				}))
				defer srv.Close()
				var p providers.Provider = openai.NewClient("test", "", srv.URL, nil, srv.Client())
				if kind == "copilot" {
					p = copilot.NewClient("test", "tid=fake", srv.URL, nil, srv.Client())
				}
				ch, err := p.Stream(context.Background(), &canonical.CanonicalRequest{Model: "test"})
				if err != nil {
					t.Fatal(err)
				}
				errors, done := 0, 0
				for ev := range ch {
					if ev.Type == canonical.EventError {
						errors++
						if ev.Error == nil {
							t.Fatal("missing error details")
						}
					}
					if ev.Type == canonical.EventMessageDone {
						done++
					}
				}
				if tt.wantError {
					if errors != 1 || done != 0 {
						t.Fatalf("errors=%d done=%d", errors, done)
					}
				} else if errors != 0 || done != 1 {
					t.Fatalf("errors=%d done=%d", errors, done)
				}
			})
		}
	}
}
func TestStreamCancellationWithFullBuffer(t *testing.T) {
	for _, kind := range []string{"openai", "copilot"} {
		t.Run(kind, func(t *testing.T) {
			sent := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", 256))
				w.(http.Flusher).Flush()
				close(sent)
				<-r.Context().Done()
			}))
			defer srv.Close()
			var p providers.Provider = openai.NewClient("test", "", srv.URL, nil, srv.Client())
			if kind == "copilot" {
				p = copilot.NewClient("test", "tid=fake", srv.URL, nil, srv.Client())
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := p.Stream(ctx, &canonical.CanonicalRequest{Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			<-sent
			deadline := time.Now().Add(time.Second)
			for len(ch) < 64 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			cancel()
			closed := make(chan struct{})
			go func() {
				for range ch {
				}
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("producer blocked after cancellation")
			}
		})
	}
}
