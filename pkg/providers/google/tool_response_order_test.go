package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/anthropic"
)

func TestToolResultImagesStayMultimodal(t *testing.T) {
	imageData := strings.Repeat("abcDEF0123+/", 10000)
	body, err := json.Marshal(map[string]any{
		"model": "gemini-3.8-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "Take a screenshot"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "shot", "name": "screenshot", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "shot", "content": []any{
				map[string]any{"type": "text", "text": "Screenshot captured"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": imageData}},
			}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var inbound anthropic.MessageRequest
	if err := json.Unmarshal(body, &inbound); err != nil {
		t.Fatal(err)
	}
	req, err := anthropic.FromAnthropicRequest(&inbound)
	if err != nil {
		t.Fatal(err)
	}
	tool := req.Messages[2].Parts[0]
	if len(tool.ToolResultParts) != 2 || tool.ToolResultText() != "Screenshot captured" || strings.Contains(tool.ToolResultContent, imageData) {
		t.Fatal("image was serialized as tool result text")
	}
	// A trip back to Anthropic must retain the nested image block.
	back, err := anthropic.ToAnthropicRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	var restored anthropic.MessageRequest
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	again, err := anthropic.FromAnthropicRequest(&restored)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Messages[2].Parts[0].ToolResultParts, tool.ToolResultParts) {
		t.Fatal("Anthropic round trip lost image")
	}
	for _, model := range []string{"gemini-3.8-flash", "gemini-2.5-pro"} {
		t.Run(model, func(t *testing.T) {
			req.Model = model
			wire, err := ToGoogleRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			last := wire.Contents[len(wire.Contents)-1]
			response := last.Parts[len(last.Parts)-1].FunctionResponse
			if response == nil || response.ID != "shot" || response.Response["output"] != "Screenshot captured" {
				t.Fatalf("tool response damaged: %+v", response)
			}
			var image *Blob
			if strings.HasPrefix(model, "gemini-3") {
				if len(response.Parts) != 1 {
					t.Fatal("missing nested tool result image")
				}
				image = response.Parts[0].InlineData
				roundTrip, err := FromGoogleRequest(wire, model)
				if err != nil {
					t.Fatal(err)
				}
				result := roundTrip.Messages[len(roundTrip.Messages)-1].Parts[0]
				if len(result.ToolResultParts) != 2 || result.ToolResultParts[1].Type != canonical.PartImage {
					t.Fatal("Google inbound lost tool image")
				}
			} else {
				if len(last.Parts) != 2 || len(response.Parts) != 0 {
					t.Fatal("older Gemini needs image outside functionResponse")
				}
				image = last.Parts[0].InlineData
			}
			if image == nil || image.Data != imageData || image.MimeType != "image/png" {
				t.Fatal("image bytes or MIME type lost")
			}
		})
	}
}

// Exercise the actual Anthropic decoder and Google HTTP client: Claude Code
// sends tool results and then a separate reminder that is merged into the turn.
func TestClaudeCodeToolResultReminderOnWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var inbound anthropic.MessageRequest
			err := json.Unmarshal([]byte(`{
    "model":"gemini-test", "max_tokens":128,
    "messages":[
     {"role":"user","content":"Calculate two sums."},
     {"role":"assistant","content":[
      {"type":"tool_use","id":"call_a","name":"add","input":{"a":2,"b":2}},
      {"type":"tool_use","id":"call_b","name":"add","input":{"a":3,"b":3}}
     ]},
     {"role":"user","content":[
      {"type":"tool_result","tool_use_id":"call_a","content":"4"},
      {"type":"text","text":"Keep both results."},
      {"type":"tool_result","tool_use_id":"call_b","content":"6","is_error":true}
     ]},
     {"role":"system","content":"Please answer briefly."}
    ]}`), &inbound)
			if err != nil {
				t.Fatal(err)
			}
			req, err := anthropic.FromAnthropicRequest(&inbound)
			if err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got GenerateContentRequest
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if len(got.Contents) != 3 {
					t.Errorf("expected 3 turns, got %d", len(got.Contents))
					w.WriteHeader(400)
					return
				}
				last := got.Contents[2]
				want := []Part{
					{Text: "Keep both results."},
					{Text: "Please answer briefly."},
					{FunctionResponse: &FunctionResponse{ID: "call_a", Name: "add", Response: map[string]any{"output": "4"}}},
					{FunctionResponse: &FunctionResponse{ID: "call_b", Name: "add", Response: map[string]any{"output": "6", "error": true}}},
				}
				if last.Role != "user" || !reflect.DeepEqual(last.Parts, want) {
					t.Errorf("unexpected tool response turn: %+v", last)
					w.WriteHeader(400)
					return
				}
				response := `{"candidates":[{"content":{"role":"model","parts":[{"text":"Done"}]},"finishReason":"STOP"}]}`
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", response)
				} else {
					fmt.Fprint(w, response)
				}
			}))
			defer upstream.Close()
			client := NewClient("google", "", upstream.URL, nil, upstream.Client())
			if stream {
				events, err := client.Stream(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				var text string
				for event := range events {
					if event.Error != nil {
						t.Fatal(event.Error)
					}
					text += event.Text
				}
				if text != "Done" {
					t.Fatalf("unexpected response %q", text)
				}
			} else {
				result, err := client.Execute(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				if result.Message.TextContent() != "Done" {
					t.Fatalf("unexpected response %+v", result)
				}
			}
		})
	}
}
