package openai

import (
	"encoding/json"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func TestTokenLimitWireParameter(t *testing.T) {
	for _, model := range []string{"gpt-6-luna", "gpt-6-sol", "gpt-6-astra", "gpt-5-mini", "gpt-4o", "gpt-oss-120b"} {
		t.Run(model, func(t *testing.T) {
			limit := 256
			req := &canonical.CanonicalRequest{Model: model}
			req.Params.MaxTokens = &limit
			out, err := ToOpenAIRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			want, absent := "max_completion_tokens", "max_tokens"
			if model == "gpt-4o" || model == "gpt-oss-120b" {
				want, absent = absent, want
			}
			if wire[want] != float64(limit) || wire[absent] != nil {
				t.Fatalf("incorrect token-limit parameter: %s", data)
			}
		})
	}
}
