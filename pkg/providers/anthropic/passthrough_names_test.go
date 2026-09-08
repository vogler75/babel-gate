package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPassthroughNamesPreserveUnknownFields(t *testing.T) {
	input := []byte(`{"tools":[{"name":"a.b","input_schema":{"type":"object"},"strict":true}],"tool_choice":{"type":"tool","name":"a.b","disable_parallel_tool_use":true},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"thought","signature":"original-signature"},{"type":"tool_use","id":"call","name":"a.b","input":{"n":9007199254740993}}]}],"extra_field":{"enabled":true}}`)
	encoded, names, err := NormalizePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	if !names.Changed() || strings.Contains(string(encoded), `"name":"a.b"`) {
		t.Fatal("name not normalized")
	}
	for _, preserved := range []string{`"strict":true`, `"disable_parallel_tool_use":true`, `"signature":"original-signature"`, `9007199254740993`, `"extra_field":{"enabled":true}`} {
		if !strings.Contains(string(encoded), preserved) {
			t.Fatalf("lost %s", preserved)
		}
	}
	for _, template := range []string{`{"type":"content_block_start","content_block":{"type":"tool_use","name":"NAME","id":"call","input":{}},"extra":true}`, `{"content":[{"type":"tool_use","name":"NAME","id":"call","input":{}}],"extra":true}`} {
		data := []byte(strings.ReplaceAll(template, "NAME", names.Name("a.b")))
		restored := RestorePayload(data, names)
		if !strings.Contains(string(restored), `"name":"a.b"`) || !json.Valid(restored) {
			t.Fatalf("not restored: %s", restored)
		}
	}
}
