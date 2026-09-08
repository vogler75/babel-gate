package openai

import (
	"github.com/vogler75/babel-gate/pkg/canonical"
	"testing"
)

func TestReasoningAliases(t *testing.T) {
	for _, data := range []string{
		`{"choices":[{"index":2,"delta":{"reasoning_content":"think","content":"answer"}}]}`,
		`{"choices":[{"index":2,"delta":{"reasoning":"think","content":"answer"}}]}`,
		`{"choices":[{"index":2,"delta":{"reasoning_content":"think","reasoning":"duplicate","content":"answer"}}]}`,
	} {
		chunk, err := UnmarshalStreamChunk([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		events := ParseOpenAIStreamEvent(chunk)
		if len(events) != 2 || events[0].Type != canonical.EventThinkingDelta || events[0].Thinking != "think" || events[1].Text != "answer" {
			t.Fatalf("lost/duplicated reasoning: %+v", events)
		}
		if events[0].CandidateIndex != 2 {
			t.Fatal("candidate lost")
		}
	}
}
