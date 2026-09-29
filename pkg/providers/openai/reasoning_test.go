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

func TestUsesMaxCompletionTokens(t *testing.T) {
	for m, want := range map[string]bool{
		"gpt-6": true, "gpt-6-mini": true, "gpt-5.1": true, "gpt-10": true, "o1": true, "o4-mini": true,
		"gpt-5-codex": true, "gpt-4o": false, "gpt-4.1": false, "gpt-3.5-turbo": false, "gpt-oss-120b": false, "llama-3": false,
	} {
		if got := usesMaxCompletionTokens(m); got != want {
			t.Errorf("%s: got %v want %v", m, got, want)
		}
	}
}
