package inbound

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers/google"
	"github.com/vogler75/babel-gate/pkg/router"
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

	sess := ResolveSession(h.sessions, r)
	estInTokens := session.EstimateRequestTokens(canonReq)

	resp, err := h.engine.Execute(r.Context(), canonReq)
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		if sess != nil && h.sessions != nil {
			h.sessions.RecordRequest(sess.ID, session.RequestRecord{
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
			Model:        canonReq.Model,
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

	sess := ResolveSession(h.sessions, r)
	estInTokens := session.EstimateRequestTokens(canonReq)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	eventsChan, err := h.engine.Stream(r.Context(), canonReq)
	if err != nil {
		if sess != nil && h.sessions != nil {
			h.sessions.RecordRequest(sess.ID, session.RequestRecord{
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

	inTokens := estInTokens
	outTokens := 0
	totalChars := 0
	streamStatus := "success"
	var streamErr string

	for ev := range eventsChan {
		if ev.Type == canonical.EventError {
			streamStatus = "error"
			if ev.Error != nil {
				streamErr = ev.Error.Error()
			}
			errData := map[string]any{
				"error": map[string]any{
					"code":    500,
					"message": ev.Error.Error(),
					"status":  "INTERNAL",
				},
			}
			b, _ := json.Marshal(errData)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()
			return
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
						"index": ev.Index,
					},
				},
			}
			b, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()
		}

		if ev.Type == canonical.EventToolCallStart {
			chunk := map[string]any{
				"candidates": []map[string]any{
					{
						"content": map[string]any{
							"role": "model",
							"parts": []map[string]any{
								{
									"functionCall": map[string]any{
										"name": ev.ToolCallName,
										"args": map[string]any{},
									},
								},
							},
						},
						"index": ev.Index,
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
							"index":        ev.Index,
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

	if sess != nil && h.sessions != nil {
		h.sessions.RecordRequest(sess.ID, session.RequestRecord{
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

func (h *GoogleHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.catalog.ListAll(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.catalog.FormatGoogle(models))
}
