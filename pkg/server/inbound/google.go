package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/google"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server/trace"
	"github.com/vogler75/babel-gate/pkg/session"
)

type GoogleHandler struct {
	engine   *router.Engine
	catalog  *router.Catalog
	sessions *session.Manager
}

func NewGoogleHandler(engine *router.Engine, catalog *router.Catalog, sessions *session.Manager) *GoogleHandler {
	return &GoogleHandler{
		engine:   engine,
		catalog:  catalog,
		sessions: sessions,
	}
}

func extractModelFromPath(path string, action string) string {
	// e.g. /v1beta/models/gemini-2.5-pro:generateContent
	idx := strings.Index(path, "/models/")
	if idx == -1 {
		return ""
	}
	sub := path[idx+len("/models/"):]
	if action != "" {
		sub = strings.TrimSuffix(sub, ":"+action)
	}
	return sub
}

func (h *GoogleHandler) HandleGenerateContent(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	tr := trace.FromContext(r.Context())
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	model := extractModelFromPath(r.URL.Path, "generateContent")
	if model == "" {
		http.Error(w, "model not specified in URL path", http.StatusBadRequest)
		return
	}

	var req google.GenerateContentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	canonReq, err := google.FromGoogleRequest(&req, model)
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
	estInTokens := session.EstimateRequestTokens(canonReq)
	prov, trackingModel := h.engine.ResolveTrackingModel(canonReq.Model)

	execStart := time.Now()
	resp, err := h.engine.Execute(r.Context(), canonReq)
	if tr != nil {
		tr.SetUpstreamDuration(time.Since(execStart))
	}
	durationMs := time.Since(startTime).Milliseconds()

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

	googResp, err := google.ToGoogleResponse(resp)
	if err != nil {
		http.Error(w, fmt.Sprintf("transform error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(googResp)
}

func (h *GoogleHandler) HandleStreamGenerateContent(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	tr := trace.FromContext(r.Context())
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	model := extractModelFromPath(r.URL.Path, "streamGenerateContent")
	if model == "" {
		http.Error(w, "model not specified in URL path", http.StatusBadRequest)
		return
	}

	var req google.GenerateContentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	canonReq, err := google.FromGoogleRequest(&req, model)
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
	estInTokens := session.EstimateRequestTokens(canonReq)
	prov, trackingModel := h.engine.ResolveTrackingModel(canonReq.Model)

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
				GenerationDurationMs: generationDurationMs(tr, time.Since(startTime)),
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
	send := func(data any) bool {
		if err := stream.data(data); err != nil {
			setFailure(fmt.Errorf("downstream Google SSE: %w", err))
			cancel()
			return false
		}
		return true
	}
	if err := stream.flush(); err != nil {
		setFailure(fmt.Errorf("downstream initial flush: %w", err))
		return
	}

	for ev := range completeToolStream(ctx, eventsChan) {
		if tr != nil && !tr.HasFirstToken() && (ev.Thinking != "" || ev.Text != "" || ev.ToolCallName != "" || ev.ToolCallID != "" || ev.Type == canonical.EventThinkingDelta || ev.Type == canonical.EventTextDelta) {
			tr.MarkFirstToken()
		}
		if ev.Type == canonical.EventError {
			setFailure(ev.Error)
			errData := map[string]any{
				"error": map[string]any{
					"code":    500,
					"message": streamErr,
					"status":  "INTERNAL",
				},
			}
			_ = stream.data(errData)
			cancel()
			return
		}

		if ev.Type == canonical.EventThinkingDelta && ev.Thinking != "" {
			totalChars += len(ev.Thinking)
			part := map[string]any{"text": ev.Thinking, "thought": true}
			if ev.ThoughtSignature != "" && (ev.ThoughtSignatureProvider == "" || ev.ThoughtSignatureProvider == canonical.SignatureProviderGoogle) {
				part["thoughtSignature"] = ev.ThoughtSignature
			}
			chunk := map[string]any{"candidates": []any{map[string]any{"index": ev.CandidateIndex, "content": map[string]any{"role": "model", "parts": []any{part}}}}}
			if !send(chunk) {
				return
			}
		}
		if ev.Type == canonical.EventTextDelta && ev.Text != "" {
			totalChars += len(ev.Text)
			chunk := map[string]any{
				"candidates": []map[string]any{
					{
						"content": map[string]any{
							"role": "model",
							"parts": []map[string]any{
								{"text": ev.Text},
							},
						},
						"index": ev.CandidateIndex,
					},
				},
			}
			if !send(chunk) {
				return
			}
		}

		if ev.Type == canonical.EventToolCallDone {
			scope := ""
			if sess != nil {
				scope = sess.ID
			}
			signature := google.ResolveThoughtSignature(ev.ThoughtSignature, ev.ThoughtSignatureProvider, scope, ev.ToolCallID)
			part := map[string]any{
				"functionCall": map[string]any{
					"id":   ev.ToolCallID,
					"name": ev.ToolCallName,
					"args": json.RawMessage(ev.ToolCallArgs),
				},
			}
			if signature != "" {
				part["thoughtSignature"] = signature
			}
			chunk := map[string]any{
				"candidates": []map[string]any{
					{
						"content": map[string]any{
							"role":  "model",
							"parts": []map[string]any{part},
						},
						"index": ev.CandidateIndex,
					},
				},
			}
			if !send(chunk) {
				return
			}
		}

		if ev.Type == canonical.EventMessageDelta {
			if ev.Usage != nil {
				usageTracker.ApplyDelta(ev.Usage)
				if !send(map[string]any{"usageMetadata": map[string]any{
					"promptTokenCount":        usageTracker.InTokens,
					"candidatesTokenCount":    usageTracker.OutTokens,
					"totalTokenCount":         usageTracker.ResolveTotal(),
					"cachedContentTokenCount": usageTracker.CachedInputTokens,
					"thoughtsTokenCount":      usageTracker.ReasoningTokens,
				}}) {
					return
				}
			}
			if ev.FinishReason != "" {
				reason := "STOP"
				if ev.FinishReason == "length" {
					reason = "MAX_TOKENS"
				}
				chunk := map[string]any{
					"candidates": []map[string]any{
						{
							"finishReason": reason,
							"index":        ev.CandidateIndex,
						},
					},
				}
				if !send(chunk) {
					return
				}
			}
		}
		if ev.Type == canonical.EventMessageDone {
			sawDone = true
		}
	}
	if !sawDone {
		if r.Context().Err() != nil {
			setFailure(r.Context().Err())
		} else {
			setFailure(io.ErrUnexpectedEOF)
			_ = stream.data(map[string]any{"error": map[string]any{"code": 500, "message": streamErr, "status": "INTERNAL"}})
		}
		return
	}
}

func (h *GoogleHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.catalog.ListAll(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.catalog.FormatGoogle(models))
}
