package openai

import (
	"github.com/vogler75/babel-gate/pkg/canonical"
	"testing"
)

func TestFallbackResponseIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		r, err := ToOpenAIResponse(&canonical.CanonicalResponse{})
		if err != nil {
			t.Fatal(err)
		}
		if r.ID == "" || seen[r.ID] {
			t.Fatalf("duplicate/empty ID %q", r.ID)
		}
		seen[r.ID] = true
	}
	r, err := ToOpenAIResponse(&canonical.CanonicalResponse{ID: "upstream"})
	if err != nil || r.ID != "upstream" {
		t.Fatalf("upstream ID changed: %+v %v", r, err)
	}
}
