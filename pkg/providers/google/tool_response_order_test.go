package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/vogler75/babel-gate/pkg/providers/anthropic"
)

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
