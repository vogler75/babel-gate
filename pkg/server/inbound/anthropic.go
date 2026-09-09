package inbound

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/providers/anthropic"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server/trace"
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

func anthropicError(err error) (int, string, string) {
	statusCode := http.StatusBadGateway
	errorType := "api_error"
	message := err.Error()
	var upstream *providers.APIError
	if !errors.As(err, &upstream) {
		return statusCode, errorType, message
	}
	message = upstream.Message
	if upstream.Provider != "" {
		message = upstream.Provider + ": " + message
	}
	switch upstream.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		statusCode, errorType = http.StatusBadRequest, "invalid_request_error"
		if strings.Contains(strings.ToLower(upstream.Message), "input token count exceeds") {
			message = "prompt is too long: " + message
		}
	case http.StatusUnauthorized:
		statusCode, errorType = http.StatusUnauthorized, "authentication_error"
	case http.StatusForbidden:
		statusCode, errorType = http.StatusForbidden, "permission_error"
	case http.StatusNotFound:
		statusCode, errorType = http.StatusNotFound, "not_found_error"
	case http.StatusRequestEntityTooLarge:
		statusCode, errorType = http.StatusRequestEntityTooLarge, "request_too_large"
	case http.StatusTooManyRequests:
		statusCode, errorType = http.StatusTooManyRequests, "rate_limit_error"
	}
	return statusCode, errorType, message
}

func writeAnthropicError(w http.ResponseWriter, err error) {
	statusCode, errorType, message := anthropicError(err)
	writeAnthropicErrorResponse(w, statusCode, errorType, message)
}

func writeAnthropicErrorResponse(w http.ResponseWriter, statusCode int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	})
}

func (h *AnthropicHandler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	tr := trace.FromContext(r.Context())
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

	if tr != nil {
		tr.SetReadDuration(time.Since(startTime))
		prov, endpoint, targetModel := h.engine.ResolveRouteInfo(req.Model)
		tr.SetRoute(req.Model, prov, endpoint, targetModel)
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

// HandleCountTokens implements Anthropic's token-counting shape. Providers
// with a native tokenizer are used when available; other providers fall back
// to BabelGate's approximate request estimator.
func (h *AnthropicHandler) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAnthropicErrorResponse(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var req anthropic.MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	canonReq, err := anthropic.FromAnthropicRequest(&req)
	if err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "invalid_request_error", "invalid request: "+err.Error())
		return
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		canonReq.AuthToken = key
	} else if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		canonReq.AuthToken = strings.TrimPrefix(auth, "Bearer ")
	}
	inputTokens, err := h.engine.CountTokens(r.Context(), canonReq)
	if errors.Is(err, router.ErrTokenCountingUnsupported) {
		inputTokens = session.EstimateRequestTokens(canonReq)
		err = nil
	}
	if err != nil {
		writeAnthropicError(w, err)
		return
	}
	// Clients count individual tools/system sections to build /context. These
	// probes are not the conversation's full prompt and must not replace its
	// context measurement or create generation sessions.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": inputTokens})
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
			prov := anthClient.Name()
			trackingModel := prov + "/" + strings.TrimPrefix(req.Model, prov+"/")
			h.sessions.RecordRequest(sess.ID, session.RequestRecord{
				Provider:             prov,
				Model:                trackingModel,
				Stream:               req.Stream,
				DurationMs:           time.Since(startTime).Milliseconds(),
				InputTokensEstimated: true,
				Status:               "error",
				ErrorMessage:         err.Error(),
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
	var upstreamUsage anthropic.Usage
	estInTokens := session.EstimateTokens(string(upstreamBody))

	tr := trace.FromContext(r.Context())
	if req.Stream {
		flusher, isFlusher := w.(http.Flusher)
		reader := bufio.NewReader(upResp.Body)
		for {
			line, rErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				if tr != nil && !tr.HasFirstToken() && bytes.HasPrefix(line, []byte("data:")) {
					tr.MarkFirstToken()
				}
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
								Usage json.RawMessage `json:"usage"`
							} `json:"message"`
							Usage json.RawMessage `json:"usage"`
						}
						if jErr := json.Unmarshal(dataPayload, &event); jErr == nil {
							for _, raw := range []json.RawMessage{event.Message.Usage, event.Usage} {
								if len(raw) > 0 {
									_ = json.Unmarshal(raw, &upstreamUsage)
								}
							}
							inTokens = upstreamUsage.TotalInputTokens()
							outTokens = upstreamUsage.OutputTokens
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
				Usage anthropic.Usage `json:"usage"`
			}
			if jErr := json.Unmarshal(respBytes, &nonStreamResp); jErr == nil {
				inTokens = nonStreamResp.Usage.TotalInputTokens()
				outTokens = nonStreamResp.Usage.OutputTokens
			}
		}
	}

	inputEstimated := inTokens == 0
	if inputEstimated {
		inTokens = estInTokens
	}
	totalTokens := inTokens + outTokens

	if tr != nil {
		tr.SetTokens(inTokens, outTokens)
		if req.Stream {
			tr.MarkStreamDone()
		} else {
			tr.SetUpstreamDuration(time.Since(startTime) - tr.ReadDuration)
		}
	}

	status := "success"
	if upResp.StatusCode >= 400 {
		status = "error"
	}
	if sess != nil && h.sessions != nil {
		prov := anthClient.Name()
		trackingModel := prov + "/" + strings.TrimPrefix(req.Model, prov+"/")
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
			Provider:             prov,
			Model:                trackingModel,
			Stream:               req.Stream,
			DurationMs:           time.Since(startTime).Milliseconds(),
			GenerationDurationMs: generationDurationMs(tr, time.Since(startTime)),
			InputTokens:          inTokens,
			InputTokensEstimated: inputEstimated,
			OutputTokens:         outTokens,
			TotalTokens:          totalTokens,
			Status:               status,
		})
	}
}

func (h *AnthropicHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, canonReq *canonical.CanonicalRequest, sess *session.Session, startTime time.Time) {
	prov, trackingModel := h.engine.ResolveTrackingModel(canonReq.Model)
	tr := trace.FromContext(r.Context())
	execStart := time.Now()
	resp, err := h.engine.Execute(r.Context(), canonReq)
	if tr != nil {
		tr.SetUpstreamDuration(time.Since(execStart))
	}
	durationMs := time.Since(startTime).Milliseconds()
	estInTokens := session.EstimateRequestTokens(canonReq)

	if err != nil {
		if sess != nil && h.sessions != nil {
			h.sessions.RecordRequest(sess.ID, session.RequestRecord{
				Provider:             prov,
				Model:                trackingModel,
				Stream:               false,
				DurationMs:           durationMs,
				InputTokens:          estInTokens,
				InputTokensEstimated: true,
				Status:               "error",
				ErrorMessage:         err.Error(),
			})
		}
		writeAnthropicError(w, err)
		return
	}

	inTokens := resp.Usage.PromptTokens
	inputEstimated := inTokens == 0
	if inTokens == 0 {
		inTokens = estInTokens
	}
	outTokens := resp.Usage.CompletionTokens
	if outTokens == 0 {
		outTokens = session.EstimateTokens(resp.Message.TextContent())
	}

	if tr != nil {
		tr.SetTokens(inTokens, outTokens)
	}

	if sess != nil && h.sessions != nil {
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
			Provider:             prov,
			Model:                trackingModel,
			Stream:               false,
			DurationMs:           durationMs,
			InputTokens:          inTokens,
			InputTokensEstimated: inputEstimated,
			OutputTokens:         outTokens,
			TotalTokens:          inTokens + outTokens,
			Status:               "success",
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
	prov, trackingModel := h.engine.ResolveTrackingModel(canonReq.Model)
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
				Provider:             prov,
				Model:                trackingModel,
				Stream:               true,
				DurationMs:           time.Since(startTime).Milliseconds(),
				InputTokens:          estInTokens,
				InputTokensEstimated: true,
				Status:               "error",
				ErrorMessage:         err.Error(),
			})
		}
		writeAnthropicError(w, err)
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
			"id":      messageID,
			"type":    "message",
			"role":    "assistant",
			"model":   modelName,
			"content": []any{},
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
	inputEstimated := true
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

	tr := trace.FromContext(r.Context())
	for ev := range completeToolStream(ctx, eventsChan) {
		if tr != nil && !tr.HasFirstToken() && (ev.Thinking != "" || ev.Text != "" || ev.ToolCallName != "" || ev.ToolCallID != "" || ev.Type == canonical.EventThinkingDelta || ev.Type == canonical.EventTextDelta) {
			tr.MarkFirstToken()
		}
		if ev.CandidateIndex != 0 {
			sendSSE("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "Anthropic output supports only candidate zero"}})
			return
		}
		if ev.Type == canonical.EventError {
			streamStatus = "error"
			translatedErr := ev.Error
			if translatedErr == nil {
				translatedErr = errors.New("upstream stream error")
			}
			streamErr = translatedErr.Error()
			_, errorType, message := anthropicError(translatedErr)
			sendSSE("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    errorType,
					"message": message,
				},
			})
			return
		}

		switch ev.Type {
		case canonical.EventMessageStart:
			if ev.Usage != nil && ev.Usage.PromptTokens > 0 {
				inTokens = ev.Usage.PromptTokens
				inputEstimated = false
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
					inputEstimated = false
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
			// Google/OpenAI may only report prompt usage at the end of a stream.
			// Anthropic clients apply this cumulative correction to message_start.
			"input_tokens":  inTokens,
			"output_tokens": outTokens,
		},
	})
	sendSSE("message_stop", map[string]any{
		"type": "message_stop",
	})

	if tr != nil {
		tr.SetTokens(inTokens, outTokens)
		tr.MarkStreamDone()
	}

	if sess != nil && h.sessions != nil {
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
			Provider:             prov,
			Model:                trackingModel,
			Stream:               true,
			DurationMs:           time.Since(startTime).Milliseconds(),
			GenerationDurationMs: generationDurationMs(tr, time.Since(startTime)),
			InputTokens:          inTokens,
			InputTokensEstimated: inputEstimated,
			OutputTokens:         outTokens,
			TotalTokens:          inTokens + outTokens,
			Status:               streamStatus,
			ErrorMessage:         streamErr,
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
