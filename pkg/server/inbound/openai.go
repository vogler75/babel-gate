package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/openai"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/session"
)

type OpenAIHandler struct {
	engine   *router.Engine
	catalog  *router.Catalog
	sessions *session.Manager
}

func NewOpenAIHandler(engine *router.Engine, catalog *router.Catalog, sessions *session.Manager) *OpenAIHandler {
	return &OpenAIHandler{
		engine:   engine,
		catalog:  catalog,
		sessions: sessions,
	}
}

func (h *OpenAIHandler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	tr := trace.FromContext(r.Context())
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req openai.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	canonReq, err := openai.FromOpenAIRequest(&req)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if tr != nil {
		tr.SetReadDuration(time.Since(startTime))
		prov, endpoint, targetModel := h.engine.ResolveRouteInfo(canonReq.Model)
		tr.SetRoute(canonReq.Model, prov, endpoint, targetModel)
	}

	sess := ResolveSession(h.sessions, r)
	if sess != nil {
		canonReq.SessionID = sess.ID
	}

	if !req.Stream {
		h.handleNonStreaming(w, r, canonReq, sess, startTime)
	} else {
		h.handleStreaming(w, r, canonReq, sess, startTime)
	}
}

func (h *OpenAIHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, canonReq *canonical.CanonicalRequest, sess *session.Session, startTime time.Time) {
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
		http.Error(w, fmt.Sprintf("router error: %v", err), http.StatusBadGateway)
		return
	}
	prov, trackingModel = executedTrackingModel(h.engine, tr, canonReq.Model)

	inTokens := resp.Usage.PromptTokens
	inputEstimated := inTokens == 0
	if inTokens == 0 {
		inTokens = estInTokens
	}
	outTokens := resp.Usage.CompletionTokens
	if outTokens == 0 {
		outTokens = session.EstimateTokens(resp.Message.TextContent())
	}
	totalTokens := resp.Usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = inTokens + outTokens
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
			CachedInputTokens:    resp.Usage.CacheReadInputTokens,
			ReasoningTokens:      resp.Usage.ReasoningTokens,
			TotalTokens:          totalTokens,
			Status:               "success",
		})
	}

	oaiResp, err := openai.ToOpenAIResponse(resp)
	if err != nil {
		http.Error(w, fmt.Sprintf("transform error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(oaiResp)
}

func (h *OpenAIHandler) handleStreaming(w http.ResponseWriter, r *http.Request, canonReq *canonical.CanonicalRequest, sess *session.Session, startTime time.Time) {
	prov, trackingModel := h.engine.ResolveTrackingModel(canonReq.Model)
	estInTokens := session.EstimateRequestTokens(canonReq)
	tr := trace.FromContext(r.Context())
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	eventsChan, err := h.engine.Stream(ctx, canonReq)
	if err != nil {
		if tr != nil {
			tr.SetTokens(estInTokens, 0)
			tr.MarkStreamDone()
		}
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
		http.Error(w, fmt.Sprintf("stream error: %v", err), http.StatusBadGateway)
		return
	}
	prov, trackingModel = executedTrackingModel(h.engine, tr, canonReq.Model)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	stream := newSSEWriter(w)

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	usageTracker := NewStreamUsageTracker(estInTokens)
	totalChars := 0
	streamStatus := "success"
	var streamErr string
	sawDone := false
	setFailure := func(err error) {
		streamStatus = "error"
		if err != nil {
			streamErr = err.Error()
		} else if streamErr == "" {
			streamErr = "stream failed"
		}
	}
	defer func() {
		usageTracker.Finalize(session.EstimateTokensFromChars(totalChars))
		if tr != nil {
			tr.SetTokens(usageTracker.InTokens, usageTracker.OutTokens)
			tr.MarkStreamDone()
		}
		if sess != nil && h.sessions != nil {
			h.sessions.RecordRequest(sess.ID, session.RequestRecord{
				Provider: prov, Model: trackingModel, Stream: true,
				DurationMs: time.Since(startTime).Milliseconds(), GenerationDurationMs: generationDurationMs(tr, time.Since(startTime)),
				InputTokens: usageTracker.InTokens, InputTokensEstimated: usageTracker.InputEstimated, OutputTokens: usageTracker.OutTokens, CachedInputTokens: usageTracker.CachedInputTokens, ReasoningTokens: usageTracker.ReasoningTokens, TotalTokens: usageTracker.TotalTokens,
				Status: streamStatus, ErrorMessage: streamErr,
			})
		}
	}()

	candidateIndex := 0
	sendChunk := func(delta map[string]any, finishReason any) bool {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   canonReq.Model,
			"choices": []map[string]any{
				{
					"index":         candidateIndex,
					"delta":         delta,
					"finish_reason": finishReason,
				},
			},
		}
		if err := stream.data(chunk); err != nil {
			setFailure(fmt.Errorf("downstream OpenAI SSE: %w", err))
			cancel()
			return false
		}
		return true
	}

	if err := stream.flush(); err != nil {
		setFailure(fmt.Errorf("downstream initial flush: %w", err))
		return
	}
	if !sendChunk(map[string]any{"role": "assistant"}, nil) {
		return
	}

	for ev := range eventsChan {
		if tr != nil && !tr.HasFirstToken() && (ev.Thinking != "" || ev.Text != "" || ev.ToolCallName != "" || ev.ToolCallID != "" || ev.Type == canonical.EventThinkingDelta || ev.Type == canonical.EventTextDelta) {
			tr.MarkFirstToken()
		}
		candidateIndex = ev.CandidateIndex
		if ev.Type == canonical.EventError {
			setFailure(ev.Error)
			errChunk := map[string]any{
				"error": map[string]any{
					"message": streamErr,
					"type":    "server_error",
				},
			}
			_ = stream.data(errChunk)
			cancel()
			return
		}

		switch ev.Type {
		case canonical.EventThinkingDelta:
			totalChars += len(ev.Thinking)
			if !sendChunk(map[string]any{"reasoning_content": ev.Thinking}, nil) {
				return
			}
		case canonical.EventTextDelta:
			totalChars += len(ev.Text)
			if !sendChunk(map[string]any{"content": ev.Text}, nil) {
				return
			}

		case canonical.EventToolCallStart:
			if !sendChunk(map[string]any{
				"tool_calls": []map[string]any{
					{
						"index": ev.Index,
						"id":    ev.ToolCallID,
						"type":  "function",
						"function": map[string]any{
							"name":      ev.ToolCallName,
							"arguments": "",
						},
					},
				},
			}, nil) {
				return
			}

		case canonical.EventToolCallDelta:
			totalChars += len(ev.ToolCallArgs)
			if !sendChunk(map[string]any{
				"tool_calls": []map[string]any{
					{
						"index": ev.Index,
						"id":    ev.ToolCallID,
						"function": map[string]any{
							"arguments": ev.ToolCallArgs,
						},
					},
				},
			}, nil) {
				return
			}

		case canonical.EventMessageDelta:
			if ev.FinishReason != "" {
				if !sendChunk(map[string]any{}, ev.FinishReason) {
					return
				}
			}
			if ev.Usage != nil {
				usageTracker.ApplyDelta(ev.Usage)
			}
		case canonical.EventMessageDone:
			sawDone = true
		}
	}
	if !sawDone {
		if r.Context().Err() != nil {
			setFailure(r.Context().Err())
		} else {
			setFailure(io.ErrUnexpectedEOF)
			_ = stream.data(map[string]any{"error": map[string]any{"message": streamErr, "type": "server_error"}})
		}
		return
	}
	usageTracker.Finalize(session.EstimateTokensFromChars(totalChars))

	usageChunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   canonReq.Model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":             usageTracker.InTokens,
			"completion_tokens":         usageTracker.OutTokens,
			"total_tokens":              usageTracker.ResolveTotal(),
			"prompt_tokens_details":     map[string]any{"cached_tokens": usageTracker.CachedInputTokens},
			"completion_tokens_details": map[string]any{"reasoning_tokens": usageTracker.ReasoningTokens},
		},
	}
	if err := stream.data(usageChunk); err != nil {
		setFailure(fmt.Errorf("downstream OpenAI usage SSE: %w", err))
		cancel()
		return
	}
	if err := stream.rawData("[DONE]"); err != nil {
		setFailure(fmt.Errorf("downstream OpenAI completion SSE: %w", err))
		cancel()
		return
	}
}

func (h *OpenAIHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.catalog.ListAll(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.catalog.FormatOpenAI(models))
}
