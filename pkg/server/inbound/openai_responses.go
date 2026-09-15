package inbound

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/openai"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/session"
)

func (h *OpenAIHandler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	if r.Method != http.MethodPost {
		writeResponsesError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	var req openai.ResponsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("invalid json: %v", err))
		return
	}
	canonReq, err := openai.FromResponsesRequest(&req)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		canonReq.AuthToken = strings.TrimPrefix(auth, "Bearer ")
	}
	tr := trace.FromContext(r.Context())
	if tr != nil {
		tr.SetReadDuration(time.Since(startTime))
		prov, endpoint, targetModel := h.engine.ResolveRouteInfo(canonReq.Model)
		tr.SetRoute(canonReq.Model, prov, endpoint, targetModel)
	}
	sess := ResolveSession(h.sessions, r)
	if sess != nil {
		canonReq.SessionID = sess.ID
	}
	if req.Stream {
		h.handleResponsesStreaming(w, r, canonReq, &req, sess, startTime)
		return
	}
	h.handleResponsesNonStreaming(w, r, canonReq, &req, sess, startTime)
}

func writeResponsesError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": message, "type": errorType, "param": nil, "code": nil,
	}})
}

func (h *OpenAIHandler) handleResponsesNonStreaming(w http.ResponseWriter, r *http.Request, req *canonical.CanonicalRequest, wireReq *openai.ResponsesRequest, sess *session.Session, started time.Time) {
	prov, trackingModel := h.engine.ResolveTrackingModel(req.Model)
	tr := trace.FromContext(r.Context())
	execStart := time.Now()
	resp, err := h.engine.Execute(r.Context(), req)
	if tr != nil {
		tr.SetUpstreamDuration(time.Since(execStart))
	}
	estIn := session.EstimateRequestTokens(req)
	if err != nil {
		recordResponsesRequest(h, sess, prov, trackingModel, false, started, estIn, true, 0, 0, 0, 0, "error", err.Error(), tr)
		writeResponsesError(w, http.StatusBadGateway, "server_error", fmt.Sprintf("router error: %v", err))
		return
	}
	prov, trackingModel = executedTrackingModel(h.engine, tr, req.Model)
	inTokens := resp.Usage.PromptTokens
	inEstimated := inTokens == 0
	if inTokens == 0 {
		inTokens = estIn
	}
	outTokens := resp.Usage.CompletionTokens
	if outTokens == 0 {
		outTokens = session.EstimateTokens(resp.Message.TextContent())
	}
	total := resp.Usage.TotalTokens
	if total == 0 {
		total = inTokens + outTokens
	}
	resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens = inTokens, outTokens, total
	if tr != nil {
		tr.SetTokens(inTokens, outTokens)
	}
	recordResponsesRequest(h, sess, prov, trackingModel, false, started, inTokens, inEstimated, outTokens, resp.Usage.CacheReadInputTokens, resp.Usage.ReasoningTokens, total, "success", "", tr)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(openai.ToResponsesResponse(resp, req.Model, wireReq))
}

func recordResponsesRequest(h *OpenAIHandler, sess *session.Session, provider, model string, stream bool, started time.Time, in int, inEstimated bool, out, cached, reasoning, total int, status, errMessage string, tr *trace.RequestTrace) {
	if sess == nil || h.sessions == nil {
		return
	}
	h.sessions.RecordRequest(sess.ID, session.RequestRecord{
		Provider: provider, Model: model, Stream: stream,
		DurationMs: time.Since(started).Milliseconds(), GenerationDurationMs: generationDurationMs(tr, time.Since(started)),
		InputTokens: in, InputTokensEstimated: inEstimated, OutputTokens: out,
		CachedInputTokens: cached, ReasoningTokens: reasoning, TotalTokens: total,
		Status: status, ErrorMessage: errMessage,
	})
}

type responsesStreamItem struct {
	item        map[string]any
	outputIndex int
	text        strings.Builder
	name        string
	kind        string
}

func (h *OpenAIHandler) handleResponsesStreaming(w http.ResponseWriter, r *http.Request, req *canonical.CanonicalRequest, wireReq *openai.ResponsesRequest, sess *session.Session, started time.Time) {
	prov, trackingModel := h.engine.ResolveTrackingModel(req.Model)
	estIn := session.EstimateRequestTokens(req)
	tr := trace.FromContext(r.Context())
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events, err := h.engine.Stream(ctx, req)
	if err != nil {
		recordResponsesRequest(h, sess, prov, trackingModel, true, started, estIn, true, 0, 0, 0, estIn, "error", err.Error(), tr)
		writeResponsesError(w, http.StatusBadGateway, "server_error", fmt.Sprintf("stream error: %v", err))
		return
	}
	prov, trackingModel = executedTrackingModel(h.engine, tr, req.Model)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	stream := newSSEWriter(w)
	responseID := "resp_" + rand.Text()
	created := time.Now().Unix()
	response := map[string]any{
		"id": responseID, "object": "response", "created_at": created, "completed_at": nil,
		"status": "in_progress", "error": nil, "incomplete_details": nil,
		"model": req.Model, "output": []any{}, "parallel_tool_calls": true, "usage": nil,
	}
	sequence := 0
	send := func(eventType string, fields map[string]any) error {
		sequence++
		fields["type"] = eventType
		fields["sequence_number"] = sequence
		return stream.event(eventType, fields)
	}
	if err := send("response.created", map[string]any{"response": response}); err != nil {
		return
	}
	if err := send("response.in_progress", map[string]any{"response": response}); err != nil {
		return
	}

	usage := NewStreamUsageTracker(estIn)
	output := make([]any, 0)
	var textItem, reasoningItem *responsesStreamItem
	tools := map[toolKey]*responsesStreamItem{}
	toolOrder := make([]toolKey, 0)
	totalChars := 0
	finishReason := "stop"
	sawDone := false
	streamStatus, streamErr := "success", ""
	newItem := func(item map[string]any) *responsesStreamItem {
		state := &responsesStreamItem{item: item, outputIndex: len(output)}
		output = append(output, item)
		_ = send("response.output_item.added", map[string]any{"output_index": state.outputIndex, "item": item})
		return state
	}
	ensureText := func() *responsesStreamItem {
		if textItem == nil {
			textItem = newItem(map[string]any{"type": "message", "id": "msg_" + rand.Text(), "status": "in_progress", "role": "assistant", "content": []any{}})
			part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}
			_ = send("response.content_part.added", map[string]any{"item_id": textItem.item["id"], "output_index": textItem.outputIndex, "content_index": 0, "part": part})
		}
		return textItem
	}
	ensureReasoning := func() *responsesStreamItem {
		if reasoningItem == nil {
			reasoningItem = newItem(map[string]any{"type": "reasoning", "id": "rs_" + rand.Text(), "status": "in_progress", "summary": []any{}})
		}
		return reasoningItem
	}
	finishText := func() {
		if textItem == nil || textItem.item["status"] == "completed" {
			return
		}
		part := map[string]any{"type": "output_text", "text": textItem.text.String(), "annotations": []any{}, "logprobs": []any{}}
		_ = send("response.output_text.done", map[string]any{"item_id": textItem.item["id"], "output_index": textItem.outputIndex, "content_index": 0, "text": textItem.text.String(), "logprobs": []any{}})
		_ = send("response.content_part.done", map[string]any{"item_id": textItem.item["id"], "output_index": textItem.outputIndex, "content_index": 0, "part": part})
		textItem.item["content"] = []any{part}
		textItem.item["status"] = "completed"
		_ = send("response.output_item.done", map[string]any{"output_index": textItem.outputIndex, "item": textItem.item})
	}
	finishReasoning := func() {
		if reasoningItem == nil || reasoningItem.item["status"] == "completed" {
			return
		}
		part := map[string]any{"type": "summary_text", "text": reasoningItem.text.String()}
		reasoningItem.item["summary"] = []any{part}
		reasoningItem.item["status"] = "completed"
		_ = send("response.reasoning_summary_text.done", map[string]any{"item_id": reasoningItem.item["id"], "output_index": reasoningItem.outputIndex, "summary_index": 0, "text": reasoningItem.text.String()})
		_ = send("response.output_item.done", map[string]any{"output_index": reasoningItem.outputIndex, "item": reasoningItem.item})
	}
	finishTool := func(state *responsesStreamItem) {
		if state == nil || state.item["status"] == "completed" {
			return
		}
		arguments := state.text.String()
		if state.kind == "custom_tool_call" {
			state.item["input"] = openai.ResponsesCustomToolInput(arguments)
		} else {
			state.item["arguments"] = arguments
		}
		state.item["status"] = "completed"
		if state.kind == "custom_tool_call" {
			_ = send("response.custom_tool_call_input.done", map[string]any{"item_id": state.item["id"], "output_index": state.outputIndex, "input": state.item["input"]})
		} else {
			_ = send("response.function_call_arguments.done", map[string]any{"item_id": state.item["id"], "output_index": state.outputIndex, "name": state.name, "arguments": arguments})
		}
		_ = send("response.output_item.done", map[string]any{"output_index": state.outputIndex, "item": state.item})
	}

	for ev := range events {
		if ev.Type == canonical.EventError {
			streamStatus, streamErr = "error", "stream failed"
			if ev.Error != nil {
				streamErr = ev.Error.Error()
			}
			_ = send("error", map[string]any{"code": "server_error", "message": streamErr, "param": nil})
			cancel()
			break
		}
		if tr != nil && !tr.HasFirstToken() && (ev.Text != "" || ev.Thinking != "" || ev.ToolCallName != "") {
			tr.MarkFirstToken()
		}
		switch ev.Type {
		case canonical.EventThinkingDelta:
			state := ensureReasoning()
			state.text.WriteString(ev.Thinking)
			totalChars += len(ev.Thinking)
			_ = send("response.reasoning_summary_text.delta", map[string]any{"item_id": state.item["id"], "output_index": state.outputIndex, "summary_index": 0, "delta": ev.Thinking})
		case canonical.EventTextDelta:
			state := ensureText()
			state.text.WriteString(ev.Text)
			totalChars += len(ev.Text)
			_ = send("response.output_text.delta", map[string]any{"item_id": state.item["id"], "output_index": state.outputIndex, "content_index": 0, "delta": ev.Text, "logprobs": []any{}})
		case canonical.EventToolCallStart:
			key := toolKey{ev.CandidateIndex, ev.Index}
			state := tools[key]
			if state == nil {
				callID := ev.ToolCallID
				if callID == "" {
					callID = "call_" + rand.Text()
				}
				item := openai.ResponsesToolCallItem(ev.ToolCallName, callID, "", "in_progress", wireReq)
				state = newItem(item)
				state.name, _ = item["name"].(string)
				state.kind, _ = item["type"].(string)
				tools[key] = state
				toolOrder = append(toolOrder, key)
			}
			state.text.WriteString(ev.ToolCallArgs)
			if ev.ToolCallArgs != "" && state.kind != "custom_tool_call" {
				totalChars += len(ev.ToolCallArgs)
				_ = send("response.function_call_arguments.delta", map[string]any{"item_id": state.item["id"], "output_index": state.outputIndex, "delta": ev.ToolCallArgs})
			}
		case canonical.EventToolCallDelta:
			key := toolKey{ev.CandidateIndex, ev.Index}
			state := tools[key]
			if state == nil {
				streamStatus, streamErr = "error", fmt.Sprintf("arguments for unknown tool index %d", ev.Index)
				_ = send("error", map[string]any{"code": "server_error", "message": streamErr, "param": nil})
				cancel()
				break
			}
			state.text.WriteString(ev.ToolCallArgs)
			totalChars += len(ev.ToolCallArgs)
			if state.kind != "custom_tool_call" {
				_ = send("response.function_call_arguments.delta", map[string]any{"item_id": state.item["id"], "output_index": state.outputIndex, "delta": ev.ToolCallArgs})
			}
		case canonical.EventToolCallDone:
			finishTool(tools[toolKey{ev.CandidateIndex, ev.Index}])
		case canonical.EventMessageDelta:
			if ev.FinishReason != "" {
				finishReason = ev.FinishReason
			}
			if ev.Usage != nil {
				usage.ApplyDelta(ev.Usage)
			}
		case canonical.EventMessageDone:
			sawDone = true
		}
		if streamStatus == "error" {
			break
		}
	}
	if streamStatus == "success" && !sawDone {
		streamStatus, streamErr = "error", io.ErrUnexpectedEOF.Error()
		_ = send("error", map[string]any{"code": "server_error", "message": streamErr, "param": nil})
	}
	if streamStatus == "success" {
		finishReasoning()
		finishText()
		for _, key := range toolOrder {
			finishTool(tools[key])
		}
		usage.Finalize(session.EstimateTokensFromChars(totalChars))
		status := "completed"
		var incomplete any
		if finishReason == "length" || finishReason == "max_tokens" {
			status = "incomplete"
			incomplete = map[string]any{"reason": "max_output_tokens"}
		}
		response["completed_at"] = time.Now().Unix()
		response["status"] = status
		response["incomplete_details"] = incomplete
		response["output"] = output
		response["usage"] = map[string]any{
			"input_tokens": usage.InTokens, "input_tokens_details": map[string]any{"cached_tokens": usage.CachedInputTokens},
			"output_tokens": usage.OutTokens, "output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
			"total_tokens": usage.ResolveTotal(),
		}
		_ = send("response.completed", map[string]any{"response": response})
	}
	usage.Finalize(session.EstimateTokensFromChars(totalChars))
	if tr != nil {
		tr.SetTokens(usage.InTokens, usage.OutTokens)
		tr.MarkStreamDone()
	}
	recordResponsesRequest(h, sess, prov, trackingModel, true, started, usage.InTokens, usage.InputEstimated, usage.OutTokens, usage.CachedInputTokens, usage.ReasoningTokens, usage.ResolveTotal(), streamStatus, streamErr, tr)
}
