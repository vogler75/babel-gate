package inbound

import (
	"context"
	"fmt"
	"github.com/vogler75/babel-gate/pkg/canonical"
	"testing"
)

func TestCompleteToolStream(t *testing.T) {
	start := func(i int, id string) canonical.CanonicalEvent {
		return canonical.CanonicalEvent{Type: canonical.EventToolCallStart, Index: i, ToolCallID: id, ToolCallName: "add"}
	}
	delta := func(i int, s string) canonical.CanonicalEvent {
		return canonical.CanonicalEvent{Type: canonical.EventToolCallDelta, Index: i, ToolCallArgs: s}
	}
	done := func(i int) canonical.CanonicalEvent {
		return canonical.CanonicalEvent{Type: canonical.EventToolCallDone, Index: i}
	}
	finish := canonical.CanonicalEvent{Type: canonical.EventMessageDelta, FinishReason: "tool_calls"}
	stop := canonical.CanonicalEvent{Type: canonical.EventMessageDone}
	for _, tt := range []struct {
		name   string
		events []canonical.CanonicalEvent
		want   []string
		bad    bool
	}{
		{"fragments", []canonical.CanonicalEvent{start(0, "a"), delta(0, `{"x":`), delta(0, `1}`), finish, stop}, []string{`a:{"x":1}`}, false},
		{"parallel same name", []canonical.CanonicalEvent{start(0, "a"), start(1, "b"), delta(1, `{"x":2}`), delta(0, `{}`), finish, stop}, []string{`a:{}`, `b:{"x":2}`}, false},
		{"explicit no duplicates", []canonical.CanonicalEvent{start(0, "a"), delta(0, `{}`), done(0), done(0), finish, stop}, []string{`a:{}`}, false},
		{"message done only", []canonical.CanonicalEvent{start(0, "a"), delta(0, `{}`), stop}, []string{`a:{}`}, false},
		{"malformed", []canonical.CanonicalEvent{start(0, "a"), delta(0, `{x}`), finish}, nil, true},
		{"null", []canonical.CanonicalEvent{start(0, "a"), delta(0, `null`), finish}, nil, true},
		{"truncated", []canonical.CanonicalEvent{start(0, "a"), delta(0, `{"x":`)}, nil, true},
		{"unconfirmed EOF", []canonical.CanonicalEvent{start(0, "a"), delta(0, `{}`)}, nil, true},
		{"upstream error", []canonical.CanonicalEvent{start(0, "a"), delta(0, `{}`), {Type: canonical.EventError, Error: fmt.Errorf("upstream failed")}}, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := make(chan canonical.CanonicalEvent, len(tt.events))
			for _, ev := range tt.events {
				input <- ev
			}
			close(input)
			var got []string
			bad := false
			finished := false
			for ev := range completeToolStream(context.Background(), input) {
				if ev.Type == canonical.EventToolCallDone {
					if finished {
						t.Fatal("tool emitted after finish")
					}
					got = append(got, ev.ToolCallID+":"+ev.ToolCallArgs)
				}
				if ev.Type == canonical.EventMessageDelta || ev.Type == canonical.EventMessageDone {
					finished = true
				}
				if ev.Type == canonical.EventError {
					bad = true
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) || bad != tt.bad {
				t.Fatalf("got=%v bad=%v", got, bad)
			}
		})
	}
}
