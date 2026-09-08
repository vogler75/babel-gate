package google

import (
	"github.com/vogler75/babel-gate/pkg/canonical"
	"testing"
)

func TestInboundToolCorrelation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		ids       []string
		results   []string
		ambiguous bool
	}{
		{"explicit reordered", []string{"a", "b"}, []string{"b", "a"}, false},
		{"idless single", []string{""}, []string{""}, false},
		{"idless explicit call", []string{"a"}, []string{""}, false},
		{"ambiguous", []string{"a", "b"}, []string{""}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, results := Content{Role: "model"}, Content{Role: "user"}
			for _, id := range tt.ids {
				calls.Parts = append(calls.Parts, Part{FunctionCall: &FunctionCall{ID: id, Name: "add", Args: map[string]any{}}})
			}
			for _, id := range tt.results {
				results.Parts = append(results.Parts, Part{FunctionResponse: &FunctionResponse{ID: id, Name: "add", Response: map[string]any{"value": 1}}})
			}
			got, err := FromGoogleRequest(&GenerateContentRequest{Contents: []Content{calls, results}}, "test")
			if tt.ambiguous {
				if err == nil {
					t.Fatal("expected ambiguous correlation error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			callIDs := map[string]bool{}
			for _, p := range got.Messages[0].Parts {
				callIDs[p.ToolCallID] = true
			}
			for i, m := range got.Messages[1:] {
				if m.Role != canonical.RoleTool || !callIDs[m.Parts[0].ToolResultID] {
					t.Fatalf("unmatched result: %+v", m)
				}
				if tt.results[i] != "" && m.Parts[0].ToolResultID != tt.results[i] {
					t.Fatal("explicit ID lost")
				}
			}
			wire, err := ToGoogleRequest(got)
			if err != nil {
				t.Fatal(err)
			}
			again, err := FromGoogleRequest(wire, "test")
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range again.Messages {
				for _, p := range m.Parts {
					if p.Type == canonical.PartToolResult && !callIDs[p.ToolResultID] {
						t.Fatal("round trip lost correlation")
					}
				}
			}
		})
	}
}
