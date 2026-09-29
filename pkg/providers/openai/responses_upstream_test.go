package openai

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func TestShouldUseResponses(t *testing.T) {
	tool := []canonical.ToolDeclaration{{Name: "x"}}
	oa := NewClient("o", "k", "", nil, nil)
	other := NewClient("o", "k", "http://localhost:8000", nil, nil)
	for _, tc := range []struct {
		c     *Client
		model string
		tools []canonical.ToolDeclaration
		want  bool
	}{
		{oa, "gpt-6", tool, true}, {oa, "gpt-5.1", tool, true}, {oa, "gpt-6", nil, false},
		{oa, "gpt-4o", tool, false}, {oa, "o3", tool, false}, {other, "gpt-6", tool, false},
	} {
		if got := tc.c.shouldUseResponses(&canonical.CanonicalRequest{Model: tc.model, Tools: tc.tools}); got != tc.want {
			t.Errorf("%s: got %v", tc.model, got)
		}
	}
}

func TestResponsesUpstreamRequest(t *testing.T) {
	n := 100
	body, _ := toResponsesUpstreamRequest(&canonical.CanonicalRequest{
		Model: "gpt-6", Stream: true, Params: canonical.Parameters{MaxTokens: &n},
		Tools:      []canonical.ToolDeclaration{{Name: "Read"}},
		ToolChoice: &canonical.ToolChoice{Mode: "named", Name: "Read"},
		Messages: []canonical.Message{
			{Role: "system", Parts: []canonical.ContentPart{{Type: "text", Text: "sys"}}},
			{Role: "user", Parts: []canonical.ContentPart{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Parts: []canonical.ContentPart{{Type: "tool_call", ToolCallID: "c1", ToolCallName: "Read"}}},
			{Role: "tool", Parts: []canonical.ContentPart{{Type: "tool_result", ToolResultID: "c1", ToolResultContent: "ok"}}},
		},
	})
	if body["instructions"] != "sys" || body["max_output_tokens"] != 100 {
		t.Fatalf("%v", body)
	}
	if tc := body["tool_choice"].(map[string]any); tc["name"] != "Read" || tc["type"] != "function" {
		t.Fatalf("%v", tc)
	}
	in := body["input"].([]map[string]any)
	if len(in) != 3 || in[1]["type"] != "function_call" || in[2]["type"] != "function_call_output" || in[1]["arguments"] != "{}" {
		t.Fatalf("%v", in)
	}
}

func TestReadResponsesStream(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r1","model":"gpt-6"}}

data: {"type":"response.output_text.delta","delta":"hi"}

data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read"}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"a\":1}"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}

`
	var types []canonical.EventType
	var last canonical.CanonicalEvent
	var finish string
	for ev := range readResponsesStream(context.Background(), io.NopCloser(strings.NewReader(sse))) {
		types = append(types, ev.Type)
		if ev.FinishReason != "" {
			finish = ev.FinishReason
		}
		if ev.Type == canonical.EventToolCallStart {
			last = ev
		}
		if ev.Type == canonical.EventError {
			t.Fatal(ev.Error)
		}
	}
	if finish != "tool_calls" || last.ToolCallID != "call_1" || last.Index != 0 || types[len(types)-1] != canonical.EventMessageDone || len(types) != 7 {
		t.Fatalf("%v %v %v", types, finish, last)
	}
	// truncated stream must error
	var gotErr bool
	for ev := range readResponsesStream(context.Background(), io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{}}\n\n"))) {
		gotErr = gotErr || ev.Type == canonical.EventError
	}
	if !gotErr {
		t.Fatal("expected error on truncation")
	}
}
