package inbound

import (
	"encoding/json"
	"fmt"
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

	var req anthropic.MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
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

	sess := ResolveSession(h.sessions, r)

	if !req.Stream {
		h.handleNonStreaming(w, r, canonReq, sess, startTime)
	} else {
		h.handleStreaming(w, r, canonReq, sess, startTime)
	}
}

func (h *AnthropicHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, canonReq *canonical.CanonicalRequest, sess *session.Session, startTime time.Time) {
	resp, err := h.engine.Execute(r.Context(), canonReq)
	durationMs := time.Since(startTime).Milliseconds()
	estInTokens := session.EstimateRequestTokens(canonReq)

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
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	estInTokens := session.EstimateRequestTokens(canonReq)
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

	for ev := range eventsChan {
		if ev.Type == canonical.EventError {
			streamStatus = "error"
			if ev.Error != nil {
				streamErr = ev.Error.Error()
			}
			sendSSE("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": ev.Error.Error(),
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

		case canonical.EventToolCallStart:
			closeActiveBlock()
			activeBlockType = "tool_use"
			stopReason = "tool_use"
			sendSSE("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": currentBlockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    ev.ToolCallID,
					"name":  ev.ToolCallName,
					"input": map[string]any{},
				},
			})

		case canonical.EventToolCallDelta:
			totalTextChars += len(ev.ToolCallArgs)
			if activeBlockType != "tool_use" {
				closeActiveBlock()
				activeBlockType = "tool_use"
				stopReason = "tool_use"
				sendSSE("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": currentBlockIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    ev.ToolCallID,
						"name":  ev.ToolCallName,
						"input": map[string]any{},
					},
				})
			}
			sendSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": currentBlockIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": ev.ToolCallArgs,
				},
			})

		case canonical.EventToolCallDone:
			closeActiveBlock()

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
