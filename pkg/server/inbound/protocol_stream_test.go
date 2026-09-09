package inbound

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/session"
)

func runProtocolStream(t *testing.T, protocol, upstreamData string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, upstreamData)
	}))
	defer srv.Close()
	engine, err := router.NewEngine(&config.Config{Providers: map[string]config.ProviderConfig{"mock": {Type: "openai", BaseURL: srv.URL, EnabledModels: []string{"test"}}}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := router.NewCatalog(engine)
	w := httptest.NewRecorder()
	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"calculate"}]}`
	switch protocol {
	case "google":
		body = `{"contents":[{"role":"user","parts":[{"text":"calculate"}]}]}`
		NewGoogleHandler(engine, catalog, nil).HandleStreamGenerateContent(w, httptest.NewRequest("POST", "/v1beta/models/test:streamGenerateContent", strings.NewReader(body)))
	case "anthropic":
		NewAnthropicHandler(engine, catalog, nil).HandleMessages(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	case "openai":
		NewOpenAIHandler(engine, catalog, nil).HandleChatCompletions(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	}
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}
func TestInboundParallelToolsAndReasoning(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"reasoning_content":"consider","reasoning":"duplicate"}}]}

data: {"choices":[{"index":0,"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"add","arguments":"{\"x\":"}},{"index":1,"id":"b","function":{"name":"add","arguments":"{"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}},{"index":0,"function":{"arguments":"1}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	for _, protocol := range []string{"google", "anthropic", "openai"} {
		t.Run(protocol, func(t *testing.T) {
			body := runProtocolStream(t, protocol, stream)
			if !strings.Contains(body, "consider") || strings.Contains(body, "duplicate") || !strings.Contains(body, "answer") {
				t.Fatalf("reasoning/text lost or duplicated: %s", body)
			}
			if strings.Contains(body, `"error"`) {
				t.Fatal(body)
			}
			switch protocol {
			case "google":
				var ids []string
				finished := false
				for _, line := range strings.Split(body, "\n") {
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					var chunk struct {
						Candidates []struct {
							Index        int
							FinishReason string
							Content      struct {
								Parts []struct {
									FunctionCall *struct {
										ID   string
										Name string
										Args map[string]any
									}
								}
							}
						}
					}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
						t.Fatal(err)
					}
					for _, c := range chunk.Candidates {
						if c.Index != 0 {
							t.Fatal("tool index leaked into candidate index")
						}
						if c.FinishReason != "" {
							finished = true
						}
						for _, p := range c.Content.Parts {
							if p.FunctionCall != nil {
								if finished {
									t.Fatal("tool after finish")
								}
								ids = append(ids, p.FunctionCall.ID)
								if p.FunctionCall.Name != "add" {
									t.Fatal("name lost")
								}
								if p.FunctionCall.ID == "a" && p.FunctionCall.Args["x"] != float64(1) {
									t.Fatal("args lost")
								}
							}
						}
					}
				}
				if fmt.Sprint(ids) != "[a b]" {
					t.Fatalf("calls lost/duplicated: %v", ids)
				}
			case "anthropic":
				active := -1
				blocks := map[int]string{}
				args := map[string]string{}
				for _, line := range strings.Split(body, "\n") {
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					var ev struct {
						Type         string
						Index        int
						ContentBlock struct{ ID string } `json:"content_block"`
						Delta        struct {
							PartialJSON string `json:"partial_json"`
						}
					}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
						t.Fatal(err)
					}
					switch ev.Type {
					case "content_block_start":
						if active != -1 {
							t.Fatal("overlapping blocks")
						}
						active = ev.Index
						blocks[ev.Index] = ev.ContentBlock.ID
					case "content_block_delta":
						if active != ev.Index {
							t.Fatal("delta for closed block")
						}
						args[blocks[ev.Index]] += ev.Delta.PartialJSON
					case "content_block_stop":
						if active != ev.Index {
							t.Fatal("wrong block closed")
						}
						active = -1
					case "message_stop":
						if active != -1 {
							t.Fatal("unclosed block")
						}
					}
				}
				if args["a"] != `{"x":1}` || args["b"] != `{}` {
					t.Fatalf("crossed arguments: %+v", args)
				}
			}
		})
	}
}
func TestInboundStreamErrors(t *testing.T) {
	for _, protocol := range []string{"google", "anthropic", "openai"} {
		for _, data := range []string{"data: {oops\n\n", `data: {"error":{"message":"quota"}}` + "\n\n", `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n" + `data: {"error":{"message":"quota"}}` + "\n\n"} {
			t.Run(protocol, func(t *testing.T) {
				body := runProtocolStream(t, protocol, data)
				if !strings.Contains(body, `"error"`) || strings.Contains(body, "[DONE]") || strings.Contains(body, "message_stop") || strings.Contains(body, "finishReason") {
					t.Fatalf("error swallowed or followed by success: %s", body)
				}
			})
		}
	}
}
func TestGoogleMultipleCandidates(t *testing.T) {
	body := runProtocolStream(t, "google", `data: {"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"other","function":{"name":"add","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
	if !strings.Contains(body, `"index":1`) || !strings.Contains(body, `"id":"other"`) || strings.Contains(body, `"error"`) {
		t.Fatal(body)
	}
}

func TestAnthropicPassthroughNormalizesNames(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Tools []struct{ Name string } }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				name := req.Tools[0].Name
				if name == "math.add" {
					t.Error("unnormalized name on passthrough")
				}
				if stream {
					fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"a\",\"name\":%q,\"input\":{}}}\n\ndata: {\"type\":\"message_stop\"}\n\n", name)
				} else {
					fmt.Fprintf(w, `{"content":[{"type":"tool_use","id":"a","name":%q,"input":{}}]}`, name)
				}
			}))
			defer upstream.Close()
			engine, err := router.NewEngine(&config.Config{Providers: map[string]config.ProviderConfig{"mock": {Type: "anthropic", BaseURL: upstream.URL, EnabledModels: []string{"test"}}}})
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"model":"test","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"test"}],"tools":[{"name":"math.add","input_schema":{"type":"object"}}]}`, stream)
			w := httptest.NewRecorder()
			NewAnthropicHandler(engine, router.NewCatalog(engine), nil).HandleMessages(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"math.add"`) {
				t.Fatalf("name not restored: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAnthropicReceivesLateGooglePromptUsageAfterRestart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello\"}]}}]}\n\n")
		// Usage arrives only after content. It must replace the initial estimate.
		fmt.Fprint(w, "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":999000,\"candidatesTokenCount\":5,\"totalTokenCount\":999005}}\n\n")
	}))
	defer upstream.Close()
	for restart := 0; restart < 2; restart++ {
		// Recreating the engine and session manager simulates a gateway restart.
		engine, err := router.NewEngine(&config.Config{Providers: map[string]config.ProviderConfig{"mock": {Type: "google", BaseURL: upstream.URL}}})
		if err != nil {
			t.Fatal(err)
		}
		sessions := session.NewManager()
		handler := NewAnthropicHandler(engine, router.NewCatalog(engine), sessions)
		request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"mock/gemini-test","stream":true,"messages":[{"role":"user","content":"full conversation supplied by client"}]}`))
		request.Header.Set("x-session-id", "same-conversation")
		w := httptest.NewRecorder()
		handler.HandleMessages(w, request)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		inputTokens, outputTokens := 0, 0
		sawCorrection := false
		for _, line := range strings.Split(w.Body.String(), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event struct {
				Type    string `json:"type"`
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Usage struct {
					InputTokens  *int `json:"input_tokens"`
					OutputTokens int  `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			if event.Type == "message_start" {
				inputTokens = event.Message.Usage.InputTokens
			}
			if event.Type == "message_delta" {
				if event.Usage.InputTokens != nil {
					inputTokens = *event.Usage.InputTokens
					sawCorrection = true
				}
				outputTokens = event.Usage.OutputTokens
			}
		}
		if !sawCorrection || inputTokens != 999000 || outputTokens != 5 {
			t.Fatalf("client kept inaccurate usage after restart %d: input=%d output=%d", restart, inputTokens, outputTokens)
		}
		got := sessions.ListSessions()
		if len(got) != 1 || got[0].ContextTokens != inputTokens || got[0].ContextTokensEstimated {
			t.Fatalf("client and dashboard context disagree: %+v", got)
		}
	}
}

func TestAnthropicPassthroughContextIncludesCachedInput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":100000,\"cache_creation_input_tokens\":7137}}}\n\n")
					fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":363,\"output_tokens\":5}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					fmt.Fprint(w, `{"usage":{"input_tokens":363,"cache_read_input_tokens":100000,"cache_creation_input_tokens":7137,"output_tokens":5}}`)
				}
			}))
			defer upstream.Close()
			engine, err := router.NewEngine(&config.Config{Providers: map[string]config.ProviderConfig{"mock": {Type: "anthropic", BaseURL: upstream.URL}}})
			if err != nil {
				t.Fatal(err)
			}
			sessions := session.NewManager()
			handler := NewAnthropicHandler(engine, router.NewCatalog(engine), sessions)
			w := httptest.NewRecorder()
			handler.HandleMessages(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"mock/claude-test","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream))))
			got := sessions.ListSessions()
			if w.Code != 200 || len(got) != 1 || got[0].ContextTokens != 107500 || got[0].ContextTokensEstimated {
				t.Fatalf("cache accounting lost: status=%d sessions=%+v", w.Code, got)
			}
		})
	}
}
