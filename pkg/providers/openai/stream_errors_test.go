package openai

import (
	"strings"
	"testing"
)

func TestStreamChunkValidation(t *testing.T) {
	for _, data := range []string{`{"error":{"message":"quota exhausted","type":"quota"}}`, `{"error":"failed"}`, `{`, `{}`, `null`, `{"choices":[{}]}`, `{"choices":[]}`} {
		if _, err := UnmarshalStreamChunk([]byte(data)); err == nil {
			t.Errorf("accepted malformed/error payload %s", data)
		}
	}
	for _, data := range []string{`{"error":null,"choices":[{"index":0,"delta":{"content":"ok"}}]}`, `{"choices":[],"usage":{"prompt_tokens":1}}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`} {
		if _, err := UnmarshalStreamChunk([]byte(data)); err != nil {
			t.Errorf("rejected valid payload %s: %v", data, err)
		}
	}
}

// Sanitized Azure-compatible gateway metadata observed on 2026-09-08.
func TestPromptFilterMetadataChunk(t *testing.T) {
	data := []byte(`{"id":"","model":"","object":"","created":0,"choices":[],"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}]}`)
	for _, data := range [][]byte{data, []byte(strings.ReplaceAll(string(data), "prompt_filter_results", "prompt_annotations"))} {
		chunk, err := UnmarshalStreamChunk(data)
		if err != nil {
			t.Fatal(err)
		}
		if events := ParseOpenAIStreamEvent(chunk); len(events) != 0 {
			t.Fatalf("metadata generated content: %+v", events)
		}
	}
}
