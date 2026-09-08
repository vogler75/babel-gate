package openai

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// ReadStream owns body and closes it and the event channel on every exit path.
// A finish reason or [DONE] is required; a bare EOF is not a successful response.
func ReadStream(ctx context.Context, body io.ReadCloser) <-chan canonical.CanonicalEvent {
	out := make(chan canonical.CanonicalEvent, 64)
	go func() {
		defer close(out)
		defer body.Close()
		stop := context.AfterFunc(ctx, func() { _ = body.Close() })
		defer stop()
		send := func(ev canonical.CanonicalEvent) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- ev:
				return true
			}
		}
		fail := func(err error) { send(canonical.CanonicalEvent{Type: canonical.EventError, Error: err}) }
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 4096), 16*1024*1024)
		var data []string
		finished := map[int]bool{}
		dispatch := func() bool {
			if len(data) == 0 {
				return true
			}
			payload := strings.Join(data, "\n")
			data = nil
			if payload == "[DONE]" {
				send(canonical.CanonicalEvent{Type: canonical.EventMessageDone})
				return false
			}
			chunk, err := UnmarshalStreamChunk([]byte(payload))
			if err != nil {
				fail(err)
				return false
			}
			for _, choice := range chunk.Choices {
				finished[choice.Index] = choice.FinishReason != "" || finished[choice.Index]
			}
			for _, ev := range ParseOpenAIStreamEvent(chunk) {
				if !send(ev) {
					return false
				}
			}
			return true
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if !dispatch() {
					return
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := scanner.Err(); err != nil {
			fail(fmt.Errorf("read completion stream: %w", err))
			return
		}
		if !dispatch() {
			return
		}
		complete := len(finished) > 0
		for _, done := range finished {
			complete = complete && done
		}
		if !complete {
			fail(io.ErrUnexpectedEOF)
			return
		}
		send(canonical.CanonicalEvent{Type: canonical.EventMessageDone})
	}()
	return out
}
