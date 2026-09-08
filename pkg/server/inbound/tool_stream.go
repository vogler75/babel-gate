package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

type toolKey struct{ candidate, index int }
type bufferedTool struct {
	event   canonical.CanonicalEvent
	args    string
	emitted bool
}

// completeToolStream buffers arguments for protocols whose tool blocks must be
// complete or sequential. Tool completion is explicit or inferred at message finish.
func completeToolStream(ctx context.Context, input <-chan canonical.CanonicalEvent) <-chan canonical.CanonicalEvent {
	out := make(chan canonical.CanonicalEvent)
	go func() {
		defer close(out)
		tools := map[toolKey]*bufferedTool{}
		finished := map[int]bool{}
		var order []toolKey
		send := func(ev canonical.CanonicalEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		fail := func(err error) { send(canonical.CanonicalEvent{Type: canonical.EventError, Error: err}) }
		emit := func(tool *bufferedTool) error {
			if tool.emitted {
				return nil
			}
			var args map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tool.args), &args); err != nil || args == nil {
				return fmt.Errorf("invalid or truncated arguments for tool %q (%s): expected a JSON object", tool.event.ToolCallName, tool.event.ToolCallID)
			}
			ev := tool.event
			ev.Type = canonical.EventToolCallDone
			ev.ToolCallArgs = tool.args
			if !send(ev) {
				return ctx.Err()
			}
			tool.emitted = true
			return nil
		}
		flush := func(candidate int, all bool) error {
			for _, key := range order {
				if all || key.candidate == candidate {
					if err := emit(tools[key]); err != nil {
						return err
					}
				}
			}
			return nil
		}
		for {
			var ev canonical.CanonicalEvent
			var ok bool
			select {
			case <-ctx.Done():
				return
			case ev, ok = <-input:
			}
			if !ok {
				if len(finished) == 0 {
					fail(io.ErrUnexpectedEOF)
					return
				}
				for _, done := range finished {
					if !done {
						fail(io.ErrUnexpectedEOF)
						return
					}
				}
				for _, tool := range tools {
					if !tool.emitted {
						fail(io.ErrUnexpectedEOF)
						return
					}
				}
				return
			}
			if ev.Type != canonical.EventMessageDone && ev.Type != canonical.EventError {
				if _, seen := finished[ev.CandidateIndex]; !seen {
					finished[ev.CandidateIndex] = false
				}
			}
			key := toolKey{ev.CandidateIndex, ev.Index}
			switch ev.Type {
			case canonical.EventError:
				send(ev)
				return
			case canonical.EventToolCallStart:
				tool := tools[key]
				if tool == nil {
					tool = &bufferedTool{event: ev}
					tools[key] = tool
					order = append(order, key)
				} else if tool.emitted {
					fail(fmt.Errorf("tool index %d reused after completion", ev.Index))
					return
				}
				if ev.ToolCallID != "" {
					tool.event.ToolCallID = ev.ToolCallID
				}
				if ev.ToolCallName != "" {
					tool.event.ToolCallName = ev.ToolCallName
				}
				if ev.ThoughtSignature != "" {
					tool.event.ThoughtSignature = ev.ThoughtSignature
				}
				tool.args += ev.ToolCallArgs
			case canonical.EventToolCallDelta:
				tool := tools[key]
				if tool == nil || tool.emitted {
					fail(fmt.Errorf("argument delta for inactive tool index %d", ev.Index))
					return
				}
				tool.args += ev.ToolCallArgs
			case canonical.EventToolCallDone:
				// Anthropic uses content_block_stop for text/thinking as well.
				if tool := tools[key]; tool != nil {
					if err := emit(tool); err != nil {
						fail(err)
						return
					}
				}
			case canonical.EventMessageDelta:
				if ev.FinishReason != "" {
					finished[ev.CandidateIndex] = true
					if err := flush(ev.CandidateIndex, false); err != nil {
						fail(err)
						return
					}
				}
				if !send(ev) {
					return
				}
			case canonical.EventMessageDone:
				if err := flush(0, true); err != nil {
					fail(err)
					return
				}
				send(ev)
				return
			default:
				if !send(ev) {
					return
				}
			}
		}
	}()
	return out
}
