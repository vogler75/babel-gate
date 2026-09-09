package inbound

import (
	"context"
	"encoding/json"
	"fmt"
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
	estInTokens := session.EstimateRequestTokens(canonReq)
	prov, trackingModel := h.engine.ResolveTrackingModel(canonReq.Model)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

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
				GenerationDurationMs: generationDurationMs(tr, time.Since(startTime)),
				InputTokens:          estInTokens,
				Status:               "error",
				ErrorMessage:         err.Error(),
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

	inTokens := estInTokens
	outTokens := 0
	totalChars := 0
	streamStatus := "success"
	var streamErr string

	for ev := range completeToolStream(ctx, eventsChan) {
		if tr != nil && !tr.HasFirstToken() && (ev.Thinking != "" || ev.Text != "" || ev.ToolCallName != "" || ev.ToolCallID != "" || ev.Type == canonical.EventThinkingDelta || ev.Type == canonical.EventTextDelta) {
			tr.MarkFirstToken()
		}
		if ev.Type == canonical.EventError {
			streamStatus = "error"
			if ev.Error != nil {
				streamErr = ev.Error.Error()
			}
			errData := map[string]any{
				"error": map[string]any{
					"code":    500,
					"message": streamErr,
					"status":  "INTERNAL",
				},
			}
			b, _ := json.Marshal(errData)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()
			return
		}

		if ev.Type == canonical.EventThinkingDelta && ev.Thinking != "" {
			totalChars += len(ev.Thinking)
			chunk := map[string]any{"candidates": []any{map[string]any{"index": ev.CandidateIndex, "content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": ev.Thinking, "thought": true}}}}}}
			b, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
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
			b, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()
		}

		if ev.Type == canonical.EventToolCallDone {
			chunk := map[string]any{
				"candidates": []map[string]any{
					{
						"content": map[string]any{
							"role": "model",
							"parts": []map[string]any{
								{
									"thoughtSignature": ev.ThoughtSignature,
									"functionCall": map[string]any{
										"id":   ev.ToolCallID,
										"name": ev.ToolCallName,
										"args": json.RawMessage(ev.ToolCallArgs),
									},
								},
							},
						},
						"index": ev.CandidateIndex,
					},
				},
			}
			b, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()
		}

		if ev.Type == canonical.EventMessageDelta {
			if ev.Usage != nil {
				if ev.Usage.PromptTokens > 0 {
					inTokens = ev.Usage.PromptTokens
				}
				if ev.Usage.CompletionTokens > 0 {
					outTokens = ev.Usage.CompletionTokens
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
				b, _ := json.Marshal(chunk)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
				flusher.Flush()
			}
		}
	}

	if outTokens == 0 {
		outTokens = session.EstimateTokens(strings.Repeat("a", totalChars))
	}
	if inTokens == 0 {
		inTokens = estInTokens
	}

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
			OutputTokens:         outTokens,
			TotalTokens:          inTokens + outTokens,
			Status:               streamStatus,
			ErrorMessage:         streamErr,
		})
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
