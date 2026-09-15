package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers"
)

type Client struct {
	name          string
	apiKey        string
	baseURL       string
	httpClient    *http.Client
	enabledModels []string
	timeouts      Timeouts
}

type Timeouts struct {
	ResponseHeader time.Duration
	StreamIdle     time.Duration
	Generation     time.Duration
}

var (
	ErrResponseHeaderTimeout = errors.New("Google response-header timeout")
	ErrStreamIdleTimeout     = errors.New("Google stream idle timeout")
	ErrGenerationTimeout     = errors.New("Google generation timeout")
)

const (
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultStreamIdleTimeout     = 120 * time.Second
	maxSSEEventBytes             = 16 * 1024 * 1024
)

func NewClient(name, apiKey, baseURL string, enabledModels []string, httpClient *http.Client) *Client {
	return NewClientWithTimeouts(name, apiKey, baseURL, enabledModels, httpClient, Timeouts{})
}

func NewClientWithTimeouts(name, apiKey, baseURL string, enabledModels []string, httpClient *http.Client, timeouts Timeouts) *Client {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") && !strings.HasSuffix(baseURL, "/v1beta") {
		baseURL = baseURL + "/v1beta"
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if timeouts.ResponseHeader == 0 {
		timeouts.ResponseHeader = defaultResponseHeaderTimeout
	} else if timeouts.ResponseHeader < 0 {
		timeouts.ResponseHeader = 0
	}
	if timeouts.StreamIdle == 0 {
		timeouts.StreamIdle = defaultStreamIdleTimeout
	} else if timeouts.StreamIdle < 0 {
		timeouts.StreamIdle = 0
	}
	if timeouts.Generation < 0 {
		timeouts.Generation = 0
	}
	return &Client{
		name:          name,
		apiKey:        apiKey,
		baseURL:       baseURL,
		httpClient:    httpClient,
		enabledModels: enabledModels,
		timeouts:      timeouts,
	}
}

func (c *Client) Name() string     { return c.name }
func (c *Client) Type() string     { return "google" }
func (c *Client) Endpoint() string { return c.baseURL }
func (c *Client) BaseURL() string  { return c.baseURL }

func (c *Client) cleanModelName(model string) string {
	model = strings.TrimPrefix(model, "google/")
	model = strings.TrimPrefix(model, "models/")
	return model
}

func (c *Client) signatureScope(sessionID, targetModel string) string {
	if sessionID == "" {
		return ""
	}
	return c.name + "\x00" + targetModel + "\x00" + sessionID
}

func (c *Client) getAPIKey(req *canonical.CanonicalRequest) string {
	if c.apiKey != "" && !strings.HasPrefix(c.apiKey, "${") {
		return c.apiKey
	}
	if req != nil && req.AuthToken != "" && !strings.HasPrefix(req.AuthToken, "dummy") && !strings.HasPrefix(req.AuthToken, "test") {
		return req.AuthToken
	}
	return c.apiKey
}

func (c *Client) apiError(operation string, statusCode int, body []byte) error {
	var envelope ErrorResponse
	_ = json.Unmarshal(body, &envelope)
	message := strings.TrimSpace(envelope.Error.Message)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return &providers.APIError{
		Provider:   c.name,
		Operation:  operation,
		StatusCode: statusCode,
		Code:       envelope.Error.Code,
		Status:     envelope.Error.Status,
		Message:    message,
	}
}

func (c *Client) requestContext(parent context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	if c.timeouts.Generation > 0 {
		timer := time.AfterFunc(c.timeouts.Generation, func() { cancel(ErrGenerationTimeout) })
		context.AfterFunc(ctx, func() { timer.Stop() })
	}
	return ctx, cancel
}

func (c *Client) doWithResponseHeaderTimeout(ctx context.Context, cancel context.CancelCauseFunc, req *http.Request) (*http.Response, error) {
	var timer *time.Timer
	if c.timeouts.ResponseHeader > 0 {
		timer = time.AfterFunc(c.timeouts.ResponseHeader, func() { cancel(ErrResponseHeaderTimeout) })
	}
	resp, err := c.httpClient.Do(req)
	if timer != nil {
		timer.Stop()
	}
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return nil, cause
		}
	}
	return resp, err
}

type idleReadCloser struct {
	body     io.ReadCloser
	timeout  time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	timedOut bool
}

func newIdleReadCloser(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return body
	}
	r := &idleReadCloser{body: body, timeout: timeout}
	r.timer = time.AfterFunc(timeout, r.expire)
	return r
}

func (r *idleReadCloser) expire() {
	r.mu.Lock()
	r.timedOut = true
	r.mu.Unlock()
	_ = r.body.Close()
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timedOut {
		return n, ErrStreamIdleTimeout
	}
	if n > 0 {
		r.timer.Stop()
		r.timer.Reset(r.timeout)
	}
	return n, err
}

func (r *idleReadCloser) Close() error {
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.body.Close()
}

func (c *Client) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	targetReq := *req
	targetReq.Stream = false
	targetModel := c.cleanModelName(targetReq.Model)
	targetReq.SessionID = c.signatureScope(targetReq.SessionID, targetModel)
	req = &targetReq
	requestCtx, cancel := c.requestContext(ctx)
	defer cancel(nil)

	googleReq, err := ToGoogleRequest(req)
	if err != nil {
		return nil, fmt.Errorf("transform to google request: %w", err)
	}

	bodyBytes, err := json.Marshal(googleReq)
	if err != nil {
		return nil, fmt.Errorf("marshal google request: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:generateContent", c.baseURL, targetModel)
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	apiKey := c.getAPIKey(req)
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("x-goog-api-key", apiKey)
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.doWithResponseHeaderTimeout(requestCtx, cancel, httpReq)
	if err != nil {
		return nil, fmt.Errorf("http execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, c.apiError("generate", resp.StatusCode, respBody)
	}

	var googleResp GenerateContentResponse
	if err := json.Unmarshal(respBody, &googleResp); err != nil {
		return nil, fmt.Errorf("unmarshal google response: %w", err)
	}

	return fromGoogleResponse(&googleResp, targetModel, googleReq.names, googleReq.signatureScope)
}

// CountTokens uses Gemini's tokenizer on the fully translated request,
// including system instructions, multimodal content, and tool declarations.
func (c *Client) CountTokens(ctx context.Context, req *canonical.CanonicalRequest) (int, error) {
	targetReq := *req
	targetReq.Stream = false
	targetModel := c.cleanModelName(targetReq.Model)
	targetReq.SessionID = c.signatureScope(targetReq.SessionID, targetModel)
	googleReq, err := ToGoogleRequest(&targetReq)
	if err != nil {
		return 0, fmt.Errorf("transform Google token count request: %w", err)
	}
	googleReq.Model = "models/" + targetModel
	googleReq.GenerationConfig = nil
	count, err := c.countTokens(ctx, req, targetModel, CountTokensRequest{GenerateContentRequest: googleReq})
	// Gemini Developer API wraps the prompt in generateContentRequest. Vertex
	// and gateways backed by Vertex accept the same fields at the top level.
	// Retry only this specific schema mismatch; never drop system/tools just
	// to make a count succeed, since that would silently undercount the prompt.
	var apiErr *providers.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(apiErr.Message, `Unknown name "generateContentRequest"`) {
		googleReq.Model = ""
		return c.countTokens(ctx, req, targetModel, googleReq)
	}
	return count, err
}

func (c *Client) countTokens(ctx context.Context, req *canonical.CanonicalRequest, targetModel string, payload any) (int, error) {
	requestCtx, cancel := c.requestContext(ctx)
	defer cancel(nil)
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal Google token count request: %w", err)
	}
	url := fmt.Sprintf("%s/models/%s:countTokens", c.baseURL, targetModel)
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return 0, fmt.Errorf("create Google token count request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	apiKey := c.getAPIKey(req)
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("x-goog-api-key", apiKey)
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := c.doWithResponseHeaderTimeout(requestCtx, cancel, httpReq)
	if err != nil {
		return 0, fmt.Errorf("Google token count request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("read Google token count response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, c.apiError("count tokens", resp.StatusCode, respBody)
	}
	var countResp struct {
		TotalTokens *int `json:"totalTokens"`
	}
	if err := json.Unmarshal(respBody, &countResp); err != nil {
		return 0, fmt.Errorf("unmarshal Google token count response: %w", err)
	}
	if countResp.TotalTokens == nil || *countResp.TotalTokens < 0 {
		return 0, fmt.Errorf("Google token count response is missing a valid totalTokens")
	}
	return *countResp.TotalTokens, nil
}

func (c *Client) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	targetReq := *req
	targetReq.Stream = true
	targetModel := c.cleanModelName(targetReq.Model)
	targetReq.SessionID = c.signatureScope(targetReq.SessionID, targetModel)
	req = &targetReq
	requestCtx, cancel := c.requestContext(ctx)

	googleReq, err := ToGoogleRequest(req)
	if err != nil {
		cancel(nil)
		return nil, fmt.Errorf("transform to google stream request: %w", err)
	}

	bodyBytes, err := json.Marshal(googleReq)
	if err != nil {
		cancel(nil)
		return nil, fmt.Errorf("marshal google request: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", c.baseURL, targetModel)
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		cancel(nil)
		return nil, fmt.Errorf("create http stream request: %w", err)
	}

	apiKey := c.getAPIKey(req)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("x-goog-api-key", apiKey)
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.doWithResponseHeaderTimeout(requestCtx, cancel, httpReq)
	if err != nil {
		cancel(nil)
		return nil, fmt.Errorf("http stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel(nil)
		apiErr := c.apiError("stream", resp.StatusCode, body)
		log.Printf("[GOOGLE STREAM ERROR] %v (target: %s)", apiErr, url)
		return nil, apiErr
	}

	eventChan := make(chan canonical.CanonicalEvent, 64)

	go func() {
		body := newIdleReadCloser(resp.Body, c.timeouts.StreamIdle)
		defer body.Close()
		defer cancel(nil)
		defer close(eventChan)
		stop := context.AfterFunc(requestCtx, func() { _ = body.Close() })
		defer stop()
		send := func(ev canonical.CanonicalEvent) bool {
			select {
			case <-requestCtx.Done():
				return false
			case eventChan <- ev:
				return true
			}
		}
		sendFinal := func(ev canonical.CanonicalEvent) bool {
			select {
			case <-ctx.Done():
				return false
			case eventChan <- ev:
				return true
			}
		}

		nextToolIndex := map[int]int{}
		toolIndexes := map[[2]int]int{}
		seenCandidate := map[int]bool{}
		terminalCandidate := map[int]bool{}
		hasToolCall := map[int]bool{}
		translatedEvents := 0
		frames, sawInput, decodeErr := decodeSSE(body, maxSSEEventBytes, func(data []byte) error {
			events, err := parseGoogleStreamEvent(data, targetModel, googleReq.names, googleReq.signatureScope)
			if err != nil {
				return err
			}
			for _, ev := range events {
				translatedEvents++
				switch ev.Type {
				case canonical.EventThinkingDelta, canonical.EventTextDelta:
					seenCandidate[ev.CandidateIndex] = true
				case canonical.EventToolCallStart, canonical.EventToolCallDelta, canonical.EventToolCallDone:
					seenCandidate[ev.CandidateIndex] = true
					hasToolCall[ev.CandidateIndex] = true
					key := [2]int{ev.CandidateIndex, ev.Index}
					if ev.Type == canonical.EventToolCallStart {
						toolIndexes[key] = nextToolIndex[ev.CandidateIndex]
						nextToolIndex[ev.CandidateIndex]++
					}
					if mapped, ok := toolIndexes[key]; ok {
						ev.Index = mapped
					}
					if ev.Type == canonical.EventToolCallDone {
						delete(toolIndexes, key)
					}
				case canonical.EventMessageDelta:
					if ev.FinishReason != "" {
						seenCandidate[ev.CandidateIndex] = true
						terminalCandidate[ev.CandidateIndex] = true
						if hasToolCall[ev.CandidateIndex] && ev.FinishReason == "stop" {
							ev.FinishReason = "tool_calls"
						}
					}
				}
				if !send(ev) {
					return requestCtx.Err()
				}
			}
			return nil
		})

		if decodeErr != nil {
			if cause := context.Cause(requestCtx); cause != nil && !errors.Is(cause, context.Canceled) {
				decodeErr = cause
			}
			sendFinal(canonical.CanonicalEvent{Type: canonical.EventError, Error: decodeErr})
			return
		}
		if requestCtx.Err() != nil {
			if cause := context.Cause(requestCtx); cause != nil && !errors.Is(cause, context.Canceled) {
				sendFinal(canonical.CanonicalEvent{Type: canonical.EventError, Error: cause})
			}
			return
		}
		if (sawInput || frames > 0) && translatedEvents == 0 {
			send(canonical.CanonicalEvent{Type: canonical.EventError, Error: errors.New("Google SSE stream contained no decodable response events")})
			return
		}
		if len(seenCandidate) == 0 {
			send(canonical.CanonicalEvent{Type: canonical.EventError, Error: io.ErrUnexpectedEOF})
			return
		}
		for index := range seenCandidate {
			if !terminalCandidate[index] {
				send(canonical.CanonicalEvent{Type: canonical.EventError, CandidateIndex: index, Error: io.ErrUnexpectedEOF})
				return
			}
		}
		send(canonical.CanonicalEvent{Type: canonical.EventMessageDone})
	}()

	return eventChan, nil
}

func (c *Client) ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	if len(c.enabledModels) > 0 {
		var list []providers.ModelInfo
		for _, m := range c.enabledModels {
			list = append(list, providers.ModelInfo{
				ID:       m,
				Name:     m,
				Provider: c.name,
			})
		}
		return list, nil
	}

	requestCtx, cancel := c.requestContext(ctx)
	defer cancel(nil)
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	apiKey := c.getAPIKey(nil)
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("x-goog-api-key", apiKey)
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.doWithResponseHeaderTimeout(requestCtx, cancel, httpReq)
	if err != nil {
		return nil, fmt.Errorf("http execute request: %w", err)
	}
	defer resp.Body.Close()

	// If /v1beta/models returned non-200 (e.g. 404 or 401) and baseURL ends with /v1beta, try fallback to /v1/models (e.g. enterprise gateway catalog)
	if resp.StatusCode != http.StatusOK && strings.HasSuffix(c.baseURL, "/v1beta") {
		altURL := strings.TrimSuffix(c.baseURL, "/v1beta") + "/v1/models"
		if altReq, err := http.NewRequestWithContext(requestCtx, http.MethodGet, altURL, nil); err == nil {
			if apiKey != "" {
				altReq.Header.Set("Authorization", "Bearer "+apiKey)
			}
			if altResp, err := c.doWithResponseHeaderTimeout(requestCtx, cancel, altReq); err == nil {
				if altResp.StatusCode == http.StatusOK {
					resp.Body.Close()
					resp = altResp
					defer resp.Body.Close()
				} else {
					altResp.Body.Close()
				}
			}
		}
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google gemini api error status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res ModelListResponse
	if err := json.Unmarshal(bodyBytes, &res); err == nil && len(res.Models) > 0 {
		var models []providers.ModelInfo
		for _, m := range res.Models {
			cleanID := strings.TrimPrefix(m.Name, "models/")
			models = append(models, providers.ModelInfo{
				ID:          cleanID,
				Name:        cleanID,
				Provider:    c.name,
				Description: m.DisplayName,
			})
		}
		return models, nil
	}

	// Try enterprise gateway catalog or generic catalog structure
	var genericMap map[string]any
	if err := json.Unmarshal(bodyBytes, &genericMap); err == nil {
		if catalogArr, ok := genericMap["catalog"].([]any); ok {
			var models []providers.ModelInfo
			seen := make(map[string]bool)
			for _, catItem := range catalogArr {
				if catMap, ok := catItem.(map[string]any); ok {
					tool, _ := catMap["tool"].(string)
					basePath, _ := catMap["base_path"].(string)
					toolLower := strings.ToLower(tool)
					if basePath == "/v1beta" || strings.Contains(toolLower, "gemini") {
						if mList, ok := catMap["models"].([]any); ok {
							for _, m := range mList {
								if mStr, ok := m.(string); ok {
									cleanID := strings.TrimPrefix(mStr, "google/")
									cleanID = strings.TrimPrefix(cleanID, "models/")
									if cleanID != "" && !seen[cleanID] {
										seen[cleanID] = true
										models = append(models, providers.ModelInfo{
											ID:       cleanID,
											Name:     cleanID,
											Provider: c.name,
										})
									}
								}
							}
						}
					}
				}
			}
			if len(models) > 0 {
				return models, nil
			}
		}

		// Try generic map with "models" or "data" array
		for _, key := range []string{"models", "data"} {
			if arr, ok := genericMap[key].([]any); ok {
				var models []providers.ModelInfo
				for _, item := range arr {
					if itemMap, ok := item.(map[string]any); ok {
						if name, ok := itemMap["name"].(string); ok && name != "" {
							cleanID := strings.TrimPrefix(name, "models/")
							models = append(models, providers.ModelInfo{ID: cleanID, Name: cleanID, Provider: c.name})
						} else if id, ok := itemMap["id"].(string); ok && id != "" {
							cleanID := strings.TrimPrefix(id, "models/")
							models = append(models, providers.ModelInfo{ID: cleanID, Name: cleanID, Provider: c.name})
						}
					} else if str, ok := item.(string); ok && str != "" {
						cleanID := strings.TrimPrefix(str, "google/")
						cleanID = strings.TrimPrefix(cleanID, "models/")
						models = append(models, providers.ModelInfo{ID: cleanID, Name: cleanID, Provider: c.name})
					}
				}
				if len(models) > 0 {
					return models, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("could not parse models from response: %s", string(bodyBytes))
}
