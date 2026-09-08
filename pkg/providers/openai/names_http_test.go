package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/providers/anthropic"
	"github.com/vogler75/babel-gate/pkg/providers/copilot"
	"github.com/vogler75/babel-gate/pkg/providers/openai"
)

func TestToolNamesOnWire(t *testing.T) {
	for _, kind := range []string{"openai", "copilot", "anthropic"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", kind, stream), func(t *testing.T) {
				original := "mcp.service/" + strings.Repeat("long-name", 10)
				req := &canonical.CanonicalRequest{Model: "test", Tools: []canonical.ToolDeclaration{{Name: original}}, ToolChoice: &canonical.ToolChoice{Mode: "named", Name: original}, Messages: []canonical.Message{{Role: "assistant", Parts: []canonical.ContentPart{{Type: canonical.PartToolCall, ToolCallID: "old", ToolCallName: original, ToolCallArgs: "{}"}}}, {Role: "tool", Parts: []canonical.ContentPart{{Type: canonical.PartToolResult, ToolResultID: "old", ToolResultContent: "ok"}}}}}
				before, _ := json.Marshal(req)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var wire map[string]any
					if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
						t.Error(err)
						return
					}
					tool := wire["tools"].([]any)[0].(map[string]any)
					name := ""
					history := ""
					selection := ""
					if kind == "anthropic" {
						name = tool["name"].(string)
						history = wire["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["name"].(string)
						selection = wire["tool_choice"].(map[string]any)["name"].(string)
					} else {
						name = tool["function"].(map[string]any)["name"].(string)
						history = wire["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
						selection = wire["tool_choice"].(map[string]any)["function"].(map[string]any)["name"].(string)
					}
					if name == original || len(name) > 64 || history != name || selection != name {
						t.Errorf("inconsistent names declaration=%q history=%q selection=%q", name, history, selection)
					}
					if kind == "anthropic" {
						if stream {
							fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"new\",\"name\":%q,\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n", name)
						} else {
							fmt.Fprintf(w, `{"id":"r","content":[{"type":"tool_use","id":"new","name":%q,"input":{}}],"stop_reason":"tool_use"}`, name)
						}
					} else if stream {
						fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"new\",\"function\":{\"name\":%q,\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", name)
					} else {
						fmt.Fprintf(w, `{"id":"r","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"new","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, name)
					}
				}))
				defer srv.Close()
				var p providers.Provider
				switch kind {
				case "openai":
					p = openai.NewClient("test", "", srv.URL, nil, srv.Client())
				case "copilot":
					p = copilot.NewClient("test", "tid=fake", srv.URL, nil, srv.Client())
				case "anthropic":
					p = anthropic.NewClient("test", "", srv.URL, nil, srv.Client())
				}
				got := ""
				if stream {
					ch, err := p.Stream(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					for ev := range ch {
						if ev.Error != nil {
							t.Fatal(ev.Error)
						}
						if ev.ToolCallName != "" {
							got = ev.ToolCallName
						}
					}
				} else {
					resp, err := p.Execute(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					for _, part := range resp.Message.Parts {
						if part.Type == canonical.PartToolCall {
							got = part.ToolCallName
						}
					}
				}
				if got != original {
					t.Fatalf("original name not restored: %q", got)
				}
				after, _ := json.Marshal(req)
				if string(before) != string(after) {
					t.Fatal("provider mutated reusable request")
				}
			})
		}
	}
}
