package toolnames

import (
	"github.com/vogler75/babel-gate/pkg/canonical"
	"reflect"
	"strings"
	"testing"
)

func TestReversibleNames(t *testing.T) {
	req := &canonical.CanonicalRequest{Tools: []canonical.ToolDeclaration{{Name: "valid-name"}, {Name: "a.b"}, {Name: "a/b"}, {Name: "a_b"}, {Name: strings.Repeat("x", 100)}, {Name: ""}}, Messages: []canonical.Message{{Role: "assistant", Parts: []canonical.ContentPart{{Type: canonical.PartToolCall, ToolCallName: "a.b"}}}}, ToolChoice: &canonical.ToolChoice{Mode: "named", Name: "a.b"}}
	limits := Constraints{MaxLength: 64, Allowed: ASCII}
	normalized, m := Normalize(req, limits)
	seen := map[string]bool{}
	for i, tool := range normalized.Tools {
		if tool.Name == "" || len(tool.Name) > 64 || seen[tool.Name] {
			t.Fatalf("invalid/colliding name %q", tool.Name)
		}
		seen[tool.Name] = true
		for _, r := range tool.Name {
			if !ASCII(r) {
				t.Fatal("invalid character")
			}
		}
		if m.Original(tool.Name) != req.Tools[i].Name {
			t.Fatal("not reversible")
		}
	}
	if normalized.Tools[0].Name != "valid-name" || normalized.Tools[3].Name != "a_b" {
		t.Fatal("valid name changed")
	}
	if normalized.Messages[0].Parts[0].ToolCallName != normalized.Tools[1].Name || normalized.ToolChoice.Name != normalized.Tools[1].Name {
		t.Fatal("inconsistent mapping")
	}
	if req.Tools[1].Name != "a.b" || req.Messages[0].Parts[0].ToolCallName != "a.b" || req.ToolChoice.Name != "a.b" {
		t.Fatal("mutated request")
	}
	// Reserve a valid name that collides exactly with the algorithm's first proposal.
	req.Tools = append(req.Tools, canonical.ToolDeclaration{Name: normalized.Tools[1].Name})
	newer, m2 := Normalize(req, limits)
	if newer.Tools[1].Name == normalized.Tools[1].Name || newer.Tools[len(newer.Tools)-1].Name != normalized.Tools[1].Name {
		t.Fatal("collision with valid name not resolved")
	}
	if m2.Original(newer.Tools[1].Name) != "a.b" {
		t.Fatal("collision not reversible")
	}
	for i := 0; i < 20; i++ {
		t.Run("concurrent", func(t *testing.T) {
			t.Parallel()
			got, _ := Normalize(req, limits)
			if !reflect.DeepEqual(got, newer) {
				t.Fatal("nondeterministic mapping")
			}
		})
	}
}
