package openai

import (
	"encoding/json"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func TestFromResponsesRequest(t *testing.T) {
	var req ResponsesRequest
	err := json.Unmarshal([]byte(`{
		"model":"gpt-5",
		"instructions":"Be concise",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"https://example.test/cat.png"}]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"},
			{"type":"function_call_output","call_id":"call_1","output":"found"}
		],
		"tools":[{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"lookup"},
		"max_output_tokens":123,
		"reasoning":{"effort":"high"}
	}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromResponsesRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-5" || got.Params.MaxTokens == nil || *got.Params.MaxTokens != 123 {
		t.Fatalf("request options lost: %+v", got)
	}
	if got.Thinking == nil || got.Thinking.Level != "high" {
		t.Fatalf("reasoning config lost: %+v", got.Thinking)
	}
	if got.ToolChoice == nil || got.ToolChoice.Mode != "named" || got.ToolChoice.Name != "lookup" {
		t.Fatalf("tool choice lost: %+v", got.ToolChoice)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "lookup" {
		t.Fatalf("tools lost: %+v", got.Tools)
	}
	if len(got.Messages) != 4 || got.Messages[0].Role != canonical.RoleSystem || got.Messages[0].TextContent() != "Be concise" {
		t.Fatalf("messages not converted: %+v", got.Messages)
	}
	if got.Messages[1].Parts[1].Type != canonical.PartImage || got.Messages[1].Parts[1].ImageURL != "https://example.test/cat.png" {
		t.Fatalf("image input lost: %+v", got.Messages[1])
	}
	if got.Messages[2].Parts[0].ToolCallID != "call_1" || got.Messages[3].Parts[0].ToolResultContent != "found" {
		t.Fatalf("function exchange lost: %+v", got.Messages)
	}
}

func TestToResponsesResponse(t *testing.T) {
	resp := &canonical.CanonicalResponse{
		Model: "gpt-5",
		Message: canonical.Message{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{
			{Type: canonical.PartText, Text: "hello"},
			{Type: canonical.PartToolCall, ToolCallID: "call_1", ToolCallName: "lookup", ToolCallArgs: `{"id":1}`},
		}},
		FinishReason: "tool_calls",
		Usage:        canonical.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7, CacheReadInputTokens: 2},
	}
	got := ToResponsesResponse(resp, "requested")
	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("invalid response envelope: %+v", got)
	}
	output := got["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "message" || output[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("invalid response output: %+v", output)
	}
	if output[1].(map[string]any)["call_id"] != "call_1" {
		t.Fatalf("call id lost: %+v", output[1])
	}
}

func TestFromResponsesRequestRejectsBuiltInTool(t *testing.T) {
	req := ResponsesRequest{Model: "gpt-5", Input: json.RawMessage(`"hi"`), Tools: []ResponsesTool{{Type: "web_search"}}}
	if _, err := FromResponsesRequest(&req); err == nil {
		t.Fatal("expected unsupported built-in tool error")
	}
}

func TestFromResponsesLiteAdditionalTools(t *testing.T) {
	var req ResponsesRequest
	err := json.Unmarshal([]byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"functions","description":"Client tools","tools":[
					{"type":"custom","name":"exec","description":"Run tool calls","format":{"type":"text"}},
					{"type":"function","name":"wait","description":"Wait","parameters":{"type":"object","properties":{}}}
				]},
				{"type":"namespace","name":"collaboration","description":"Agent tools","tools":[
					{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}
				]}
			]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex"}]},
			{"type":"message","role":"user","content":"hello"}
		]
	}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromResponsesRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 3 {
		t.Fatalf("additional tools not promoted: %+v", got.Tools)
	}
	if got.Tools[0].Name != "exec" || got.Tools[1].Name != "wait" || got.Tools[2].Name != "collaboration__spawn_agent" {
		t.Fatalf("namespace flattening is wrong: %+v", got.Tools)
	}
	if info, ok := req.ToolInfo("exec"); !ok || info.Kind != "custom" || info.Namespace != "functions" || info.Name != "exec" {
		t.Fatalf("custom tool metadata lost: %+v, %v", info, ok)
	}
	if info, ok := req.ToolInfo("collaboration__spawn_agent"); !ok || info.Namespace != "collaboration" || info.Name != "spawn_agent" {
		t.Fatalf("namespaced function metadata lost: %+v, %v", info, ok)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != canonical.RoleSystem || got.Messages[1].Role != canonical.RoleUser {
		t.Fatalf("additional_tools leaked into prompt messages: %+v", got.Messages)
	}

	custom := ResponsesToolCallItem("exec", "call_1", `{"input":"await tools.exec_command({cmd: 'ls'})"}`, "completed", &req)
	if custom["type"] != "custom_tool_call" || custom["namespace"] != "functions" || custom["name"] != "exec" || custom["input"] != "await tools.exec_command({cmd: 'ls'})" {
		t.Fatalf("custom call metadata not restored: %+v", custom)
	}
	function := ResponsesToolCallItem("collaboration__spawn_agent", "call_2", `{}`, "completed", &req)
	if function["type"] != "function_call" || function["namespace"] != "collaboration" || function["name"] != "spawn_agent" {
		t.Fatalf("function namespace not restored: %+v", function)
	}
}
