package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
				Provider:     prov,
				Model:        trackingModel,
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

	if tr != nil {
		tr.SetTokens(inTokens, outTokens)
	}

	if sess != nil && h.sessions != nil {
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
			Provider:     prov,
			Model:        trackingModel,
			Stream:       false,
			DurationMs:   durationMs,
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			TotalTokens:  inTokens + outTokens,
			Status:       "success",
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
				Model:        trackingModel,
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

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	inTokens := estInTokens
	outTokens := 0
	totalChars := 0
	streamStatus := "success"
	var streamErr string

	candidateIndex := 0
	sendChunk := func(delta map[string]any, finishReason any) {
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
		b, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
	}

	// Initial role event
	sendChunk(map[string]any{"role": "assistant"}, nil)

	tr := trace.FromContext(r.Context())
	for ev := range eventsChan {
		if tr != nil && !tr.HasFirstToken() && (ev.Thinking != "" || ev.Text != "" || ev.ToolCallName != "" || ev.ToolCallID != "" || ev.Type == canonical.EventThinkingDelta || ev.Type == canonical.EventTextDelta) {
			tr.MarkFirstToken()
		}
		candidateIndex = ev.CandidateIndex
		if ev.Type == canonical.EventError {
			streamStatus = "error"
			if ev.Error != nil {
				streamErr = ev.Error.Error()
			}
			errChunk := map[string]any{
				"error": map[string]any{
					"message": streamErr,
					"type":    "server_error",
				},
			}
			b, _ := json.Marshal(errChunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()
			return
		}

		switch ev.Type {
		case canonical.EventThinkingDelta:
			totalChars += len(ev.Thinking)
			sendChunk(map[string]any{"reasoning_content": ev.Thinking}, nil)
		case canonical.EventTextDelta:
			totalChars += len(ev.Text)
			sendChunk(map[string]any{"content": ev.Text}, nil)

		case canonical.EventToolCallStart:
			sendChunk(map[string]any{
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
			}, nil)

		case canonical.EventToolCallDelta:
			totalChars += len(ev.ToolCallArgs)
			sendChunk(map[string]any{
				"tool_calls": []map[string]any{
					{
						"index": ev.Index,
						"id":    ev.ToolCallID,
						"function": map[string]any{
							"arguments": ev.ToolCallArgs,
						},
					},
				},
			}, nil)

		case canonical.EventMessageDelta:
			if ev.FinishReason != "" {
				sendChunk(map[string]any{}, ev.FinishReason)
			}
			if ev.Usage != nil {
				if ev.Usage.PromptTokens > 0 {
					inTokens = ev.Usage.PromptTokens
				}
				if ev.Usage.CompletionTokens > 0 {
					outTokens = ev.Usage.CompletionTokens
				}
			}
		}
	}

	if outTokens == 0 {
		outTokens = session.EstimateTokens(strings.Repeat("a", totalChars))
	}
	if inTokens == 0 {
		inTokens = estInTokens
	}

	// Send usage chunk before DONE
	usageChunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   canonReq.Model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     inTokens,
			"completion_tokens": outTokens,
			"total_tokens":      inTokens + outTokens,
		},
	}
	bUsage, _ := json.Marshal(usageChunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", string(bUsage))

	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	if tr != nil {
		tr.SetTokens(inTokens, outTokens)
		tr.MarkStreamDone()
	}

	if sess != nil && h.sessions != nil {
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
			Provider:     prov,
			Model:        trackingModel,
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

func (h *OpenAIHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.catalog.ListAll(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.catalog.FormatOpenAI(models))
}
