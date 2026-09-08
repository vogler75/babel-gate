package inbound

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/anthropic"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/session"
)

type AnthropicHandler struct {
	engine   *router.Engine
	catalog  *router.Catalog
	sessions *session.Manager
}

func NewAnthropicHandler(engine *router.Engine, catalog *router.Catalog, sessions *session.Manager) *AnthropicHandler {
	return &AnthropicHandler{
		engine:   engine,
		catalog:  catalog,
		sessions: sessions,
	}
}

func (h *AnthropicHandler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read request body: %v", err), http.StatusBadRequest)
		return
	}

	var req anthropic.MessageRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	sess := ResolveSession(h.sessions, r)

	// Direct 1:1 passthrough when routing Anthropic client protocol to an Anthropic upstream provider
	if route, err := h.engine.ResolveModel(req.Model); err == nil && route != nil && route.Provider.Type() == "anthropic" {
		if anthClient, ok := route.Provider.(*anthropic.Client); ok {
			h.handlePassthrough(w, r, anthClient, route.TargetModel, &req, bodyBytes, sess, startTime)
			return
		}
	}

	canonReq, err := anthropic.FromAnthropicRequest(&req)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if key := r.Header.Get("x-api-key"); key != "" {
		canonReq.AuthToken = key
	} else if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		canonReq.AuthToken = strings.TrimPrefix(auth, "Bearer ")
	}

	if !req.Stream {
		h.handleNonStreaming(w, r, canonReq, sess, startTime)
	} else {
		h.handleStreaming(w, r, canonReq, sess, startTime)
	}
}

// sanitizeAnthropicPayload cleanses incoming messages so Anthropic / Vertex validation rules pass:
// - Rewrites target model if configured.
// - Replaces empty thinking blocks with signature into standard redacted_thinking blocks.
// - Removes empty thinking blocks without signature.
func sanitizeAnthropicPayload(bodyBytes []byte, targetModel string, originalModel string) []byte {
	var rawMap map[string]any
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		return bodyBytes
	}

	if targetModel != "" && targetModel != originalModel {
		rawMap["model"] = targetModel
	}

	messagesRaw, ok := rawMap["messages"].([]any)
	if !ok {
		reencoded, err := json.Marshal(rawMap)
		if err == nil {
			return reencoded
		}
		return bodyBytes
	}

	for _, mRaw := range messagesRaw {
		mMap, ok := mRaw.(map[string]any)
		if !ok {
			continue
		}
		contentRaw, ok := mMap["content"]
		if !ok {
			continue
		}

		blocks, ok := contentRaw.([]any)
		if !ok {
			continue
		}

		var sanitizedBlocks []any
		for _, bRaw := range blocks {
			bMap, ok := bRaw.(map[string]any)
			if !ok {
				sanitizedBlocks = append(sanitizedBlocks, bRaw)
				continue
			}

			bType, _ := bMap["type"].(string)
			if bType == "thinking" {
				th, _ := bMap["thinking"].(string)
				sig, _ := bMap["signature"].(string)
				if strings.TrimSpace(th) == "" {
					if sig != "" {
						// Convert empty thinking with signature to standard redacted_thinking
						sanitizedBlocks = append(sanitizedBlocks, map[string]any{
							"type": "redacted_thinking",
							"data": sig,
						})
					}
					// If signature is empty too, drop the corrupted empty block completely
					continue
				}
			}
			sanitizedBlocks = append(sanitizedBlocks, bMap)
		}

		if len(sanitizedBlocks) == 0 {
			sanitizedBlocks = append(sanitizedBlocks, map[string]any{
				"type": "text",
				"text": " ",
			})
		}
		mMap["content"] = sanitizedBlocks
	}

	rawMap["messages"] = messagesRaw
	reencoded, err := json.Marshal(rawMap)
	if err != nil {
		return bodyBytes
	}
	return reencoded
}

func (h *AnthropicHandler) handlePassthrough(
	w http.ResponseWriter,
	r *http.Request,
	anthClient *anthropic.Client,
	targetModel string,
	req *anthropic.MessageRequest,
	bodyBytes []byte,
	sess *session.Session,
	startTime time.Time,
) {
	clientToken := ""
	if key := r.Header.Get("x-api-key"); key != "" {
		clientToken = key
	} else if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		clientToken = strings.TrimPrefix(auth, "Bearer ")
	}
	apiKey := anthClient.ResolveAPIKey(clientToken)

	upstreamBody := sanitizeAnthropicPayload(bodyBytes, targetModel, req.Model)
	upstreamBody, names, err := anthropic.NormalizePayload(upstreamBody)
	if err != nil {
		http.Error(w, "invalid tool names payload", http.StatusBadRequest)
		return
	}

	targetURL := anthClient.BaseURL() + "/messages"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	if err != nil {
		http.Error(w, fmt.Sprintf("create upstream request: %v", err), http.StatusInternalServerError)
		return
	}

	for headerName, values := range r.Header {
		lower := strings.ToLower(headerName)
		if lower == "content-length" || lower == "host" || lower == "connection" {
			continue
		}
		for _, v := range values {
			upReq.Header.Add(headerName, v)
		}
	}

	if apiKey != "" {
		upReq.Header.Set("x-api-key", apiKey)
		upReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if upReq.Header.Get("anthropic-version") == "" {
		upReq.Header.Set("anthropic-version", "2023-06-01")
	}
	upReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		upReq.Header.Set("Accept", "text/event-stream")
	}

	upResp, err := anthClient.HTTPClient().Do(upReq)
	if err != nil {
		if sess != nil && h.sessions != nil {
			h.sessions.RecordRequest(sess.ID, session.RequestRecord{
				Provider:     anthClient.Name(),
				Model:        req.Model,
				Stream:       req.Stream,
				DurationMs:   time.Since(startTime).Milliseconds(),
				Status:       "error",
				ErrorMessage: err.Error(),
			})
		}
		http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer upResp.Body.Close()

	for k, vv := range upResp.Header {
		if names.Changed() && strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upResp.StatusCode)

	inTokens := 0
	outTokens := 0
	estInTokens := session.EstimateTokens(string(upstreamBody))

	if req.Stream {
		flusher, isFlusher := w.(http.Flusher)
		reader := bufio.NewReader(upResp.Body)
		for {
			line, rErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				if names.Changed() && bytes.HasPrefix(line, []byte("data:")) {
					payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
					line = append([]byte("data: "), anthropic.RestorePayload(payload, names)...)
					line = append(line, '\n')
				}
				if _, err := w.Write(line); err != nil {
					return
				}
				if isFlusher {
					flusher.Flush()
				}

				if bytes.HasPrefix(line, []byte("data: ")) {
					dataPayload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data: ")))
					if bytes.Contains(dataPayload, []byte(`"usage"`)) {
						var event struct {
							Type    string `json:"type"`
							Message struct {
								Usage struct {
									InputTokens  int `json:"input_tokens"`
									OutputTokens int `json:"output_tokens"`
								} `json:"usage"`
							} `json:"message"`
							Usage struct {
								InputTokens  int `json:"input_tokens"`
								OutputTokens int `json:"output_tokens"`
							} `json:"usage"`
						}
						if jErr := json.Unmarshal(dataPayload, &event); jErr == nil {
							if event.Message.Usage.InputTokens > 0 {
								inTokens = event.Message.Usage.InputTokens
							}
							if event.Usage.InputTokens > 0 {
								inTokens = event.Usage.InputTokens
							}
							if event.Message.Usage.OutputTokens > 0 {
								outTokens = event.Message.Usage.OutputTokens
							}
							if event.Usage.OutputTokens > 0 {
								outTokens = event.Usage.OutputTokens
							}
						}
					}
				}
			}
			if rErr != nil {
				break
			}
		}
	} else {
		respBytes, _ := io.ReadAll(upResp.Body)
		respBytes = anthropic.RestorePayload(respBytes, names)
		if len(respBytes) > 0 {
			_, _ = w.Write(respBytes)
			var nonStreamResp struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if jErr := json.Unmarshal(respBytes, &nonStreamResp); jErr == nil {
				inTokens = nonStreamResp.Usage.InputTokens
				outTokens = nonStreamResp.Usage.OutputTokens
			}
		}
	}

	if inTokens == 0 {
		inTokens = estInTokens
	}
	totalTokens := inTokens + outTokens

	status := "success"
	if upResp.StatusCode >= 400 {
		status = "error"
	}
	if sess != nil && h.sessions != nil {
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
			Provider:     anthClient.Name(),
			Model:        req.Model,
			Stream:       req.Stream,
			DurationMs:   time.Since(startTime).Milliseconds(),
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			TotalTokens:  totalTokens,
			Status:       status,
		})
	}
}

func (h *AnthropicHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, canonReq *canonical.CanonicalRequest, sess *session.Session, startTime time.Time) {
	prov := h.engine.ResolveProviderName(canonReq.Model)
	resp, err := h.engine.Execute(r.Context(), canonReq)
	durationMs := time.Since(startTime).Milliseconds()
	estInTokens := session.EstimateRequestTokens(canonReq)

	if err != nil {
		if sess != nil && h.sessions != nil {
			h.sessions.RecordRequest(sess.ID, session.RequestRecord{
				Provider:     prov,
				Model:        canonReq.Model,
				Stream:       false,
				DurationMs:   durationMs,
				InputTokens:  estInTokens,
				Status:       "error",
				ErrorMessage: err.Error(),
			})
		}
		http.Error(w, fmt.Sprintf("router error: %v", err), http.StatusBadGateway)
		return
	}

	inTokens := resp.Usage.PromptTokens
	if inTokens == 0 {
		inTokens = estInTokens
	}
	outTokens := resp.Usage.CompletionTokens
	if outTokens == 0 {
		outTokens = session.EstimateTokens(resp.Message.TextContent())
	}

	if sess != nil && h.sessions != nil {
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
			Provider:     prov,
			Model:        canonReq.Model,
			Stream:       false,
			DurationMs:   durationMs,
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			TotalTokens:  inTokens + outTokens,
			Status:       "success",
		})
	}

	anthResp, err := anthropic.ToAnthropicResponse(resp)
	if err != nil {
		http.Error(w, fmt.Sprintf("transform error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(anthResp)
}

func (h *AnthropicHandler) handleStreaming(w http.ResponseWriter, r *http.Request, canonReq *canonical.CanonicalRequest, sess *session.Session, startTime time.Time) {
	prov := h.engine.ResolveProviderName(canonReq.Model)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	estInTokens := session.EstimateRequestTokens(canonReq)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	eventsChan, err := h.engine.Stream(ctx, canonReq)
	if err != nil {
		if sess != nil && h.sessions != nil {
			h.sessions.RecordRequest(sess.ID, session.RequestRecord{
				Provider:     prov,
				Model:        canonReq.Model,
				Stream:       true,
				DurationMs:   time.Since(startTime).Milliseconds(),
				InputTokens:  estInTokens,
				Status:       "error",
				ErrorMessage: err.Error(),
			})
		}
		http.Error(w, fmt.Sprintf("stream error: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sendSSE := func(eventType string, data any) {
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
		flusher.Flush()
	}

	messageID := fmt.Sprintf("msg_%d", time.Now().UnixMilli())
	modelName := canonReq.Model

	// 1. Send message_start with estimated/known input tokens
	sendSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    messageID,
			"type":  "message",
			"role":  "assistant",
			"model": modelName,
			"usage": map[string]any{
				"input_tokens":  estInTokens,
				"output_tokens": 0,
			},
		},
	})

	currentBlockIndex := 0
	activeBlockType := "" // "text", "thinking", "tool_use"
	stopReason := "end_turn"
	inTokens := estInTokens
	outTokens := 0
	totalTextChars := 0
	streamStatus := "success"
	var streamErr string

	closeActiveBlock := func() {
		if activeBlockType != "" {
			sendSSE("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": currentBlockIndex,
			})
			currentBlockIndex++
			activeBlockType = ""
		}
	}

	for ev := range completeToolStream(ctx, eventsChan) {
		if ev.CandidateIndex != 0 {
			sendSSE("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "Anthropic output supports only candidate zero"}})
			return
		}
		if ev.Type == canonical.EventError {
			streamStatus = "error"
			if ev.Error != nil {
				streamErr = ev.Error.Error()
			}
			sendSSE("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": streamErr,
				},
			})
			return
		}

		switch ev.Type {
		case canonical.EventMessageStart:
			if ev.Usage != nil && ev.Usage.PromptTokens > 0 {
				inTokens = ev.Usage.PromptTokens
			}

		case canonical.EventThinkingDelta:
			totalTextChars += len(ev.Thinking)
			if activeBlockType != "thinking" {
				closeActiveBlock()
				activeBlockType = "thinking"
				sendSSE("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": currentBlockIndex,
					"content_block": map[string]any{
						"type":     "thinking",
						"thinking": "",
					},
				})
			}
			sendSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": currentBlockIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": ev.Thinking,
				},
			})

		case canonical.EventTextDelta:
			totalTextChars += len(ev.Text)
			if activeBlockType != "text" {
				closeActiveBlock()
				activeBlockType = "text"
				sendSSE("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": currentBlockIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			sendSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": currentBlockIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": ev.Text,
				},
			})

		case canonical.EventToolCallDone:
			closeActiveBlock()
			stopReason = "tool_use"
			sendSSE("content_block_start", map[string]any{
				"type": "content_block_start", "index": currentBlockIndex,
				"content_block": map[string]any{"type": "tool_use", "id": ev.ToolCallID, "name": ev.ToolCallName, "input": map[string]any{}},
			})
			totalTextChars += len(ev.ToolCallArgs)
			sendSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": currentBlockIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ToolCallArgs}})
			sendSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": currentBlockIndex})
			currentBlockIndex++

		case canonical.EventMessageDelta:
			if ev.FinishReason != "" {
				if ev.FinishReason == "tool_calls" {
					stopReason = "tool_use"
				} else if ev.FinishReason == "length" {
					stopReason = "max_tokens"
				} else {
					stopReason = "end_turn"
				}
			}
			if ev.Usage != nil {
				if ev.Usage.PromptTokens > 0 {
					inTokens = ev.Usage.PromptTokens
				}
				if ev.Usage.CompletionTokens > 0 {
					outTokens = ev.Usage.CompletionTokens
				}
			}

		case canonical.EventMessageDone:
			closeActiveBlock()
		}
	}

	closeActiveBlock()

	if outTokens == 0 {
		outTokens = session.EstimateTokens(strings.Repeat("a", totalTextChars))
	}
	if inTokens == 0 {
		inTokens = estInTokens
	}

	// Final message_delta & message_stop
	sendSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": outTokens,
		},
	})
	sendSSE("message_stop", map[string]any{
		"type": "message_stop",
	})

	if sess != nil && h.sessions != nil {
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
			Provider:     prov,
			Model:        canonReq.Model,
			Stream:       true,
			DurationMs:   time.Since(startTime).Milliseconds(),
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			TotalTokens:  inTokens + outTokens,
			Status:       streamStatus,
			ErrorMessage: streamErr,
		})
	}
}

func (h *AnthropicHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.catalog.ListAll(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.catalog.FormatAnthropic(models))
}
