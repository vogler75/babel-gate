package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers"
)

type Client struct {
	name          string
	apiKey        string
	baseURL       string
	httpClient    *http.Client
	enabledModels []string
}

func NewClient(name, apiKey, baseURL string, enabledModels []string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL = baseURL + "/v1"
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		name:          name,
		apiKey:        apiKey,
		baseURL:       baseURL,
		httpClient:    httpClient,
		enabledModels: enabledModels,
	}
}

func (c *Client) Name() string { return c.name }
func (c *Client) Type() string { return "anthropic" }

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) HTTPClient() *http.Client {
	if c.httpClient == nil {
		return http.DefaultClient
	}
	return c.httpClient
}

func (c *Client) ResolveAPIKey(clientToken string) string {
	if c.apiKey != "" && !strings.HasPrefix(c.apiKey, "${") {
		return c.apiKey
	}
	if clientToken != "" && !strings.HasPrefix(clientToken, "dummy") && !strings.HasPrefix(clientToken, "test") {
		return clientToken
	}
	return c.apiKey
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

func (c *Client) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	req.Stream = false
	anthropicReq, err := ToAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("transform to anthropic request: %w", err)
	}

	bodyBytes, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	apiKey := c.getAPIKey(req)
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic api error status %d: %s", resp.StatusCode, string(respBody))
	}

	var anthropicResp MessageResponse
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("unmarshal anthropic response: %w", err)
	}

	return FromAnthropicResponse(&anthropicResp)
}

func (c *Client) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	req.Stream = true
	anthropicReq, err := ToAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("transform to anthropic stream request: %w", err)
	}

	bodyBytes, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create http stream request: %w", err)
	}

	apiKey := c.getAPIKey(req)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic stream api error %d: %s", resp.StatusCode, string(body))
	}

	eventChan := make(chan canonical.CanonicalEvent, 64)

	go func() {
		defer resp.Body.Close()
		defer close(eventChan)

		scanner := bufio.NewScanner(resp.Body)
		var currentEvent string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				currentEvent = strings.TrimPrefix(line, "event: ")
				continue
			}
			if strings.HasPrefix(line, "data: ") {
				dataStr := strings.TrimPrefix(line, "data: ")
				events, err := ParseAnthropicStreamEvent([]byte(dataStr))
				if err != nil {
					continue
				}
				for _, ev := range events {
					select {
					case <-ctx.Done():
						eventChan <- canonical.CanonicalEvent{Type: canonical.EventError, Error: ctx.Err()}
						return
					case eventChan <- ev:
					}
				}
				_ = currentEvent
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			eventChan <- canonical.CanonicalEvent{Type: canonical.EventError, Error: err}
		}
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	apiKey := c.getAPIKey(nil)
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic api error status %d: %s", resp.StatusCode, string(respBody))
	}

	var anthResp struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &anthResp); err == nil && len(anthResp.Data) > 0 {
		var models []providers.ModelInfo
		for _, m := range anthResp.Data {
			name := m.DisplayName
			if name == "" {
				name = m.ID
			}
			models = append(models, providers.ModelInfo{
				ID:       m.ID,
				Name:     name,
				Provider: c.name,
			})
		}
		return models, nil
	}

	// Try enterprise gateway catalog structure: {"catalog": [{"tool": "...", "base_path": "...", "models": [...]}]}
	var genericMap map[string]any
	if err := json.Unmarshal(respBody, &genericMap); err == nil {
		if catalogArr, ok := genericMap["catalog"].([]any); ok {
			var models []providers.ModelInfo
			seen := make(map[string]bool)
			for _, catItem := range catalogArr {
				if catMap, ok := catItem.(map[string]any); ok {
					tool, _ := catMap["tool"].(string)
					basePath, _ := catMap["base_path"].(string)
					toolLower := strings.ToLower(tool)
					if strings.Contains(basePath, "messages") || strings.Contains(toolLower, "anthropic") || strings.Contains(toolLower, "claude") {
						if mList, ok := catMap["models"].([]any); ok {
							for _, m := range mList {
								if mStr, ok := m.(string); ok && !seen[mStr] {
									seen[mStr] = true
									models = append(models, providers.ModelInfo{
										ID:       mStr,
										Name:     mStr,
										Provider: c.name,
									})
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
	}

	// Fallback to standard Claude models list if 200 OK but unparsed
	return []providers.ModelInfo{
		{ID: "claude-3-7-sonnet-20250219", Name: "claude-3-7-sonnet", Provider: c.name, Description: "Claude 3.7 Sonnet"},
		{ID: "claude-3-5-sonnet-20241022", Name: "claude-3-5-sonnet", Provider: c.name, Description: "Claude 3.5 Sonnet"},
		{ID: "claude-3-5-haiku-20241022", Name: "claude-3-5-haiku", Provider: c.name, Description: "Claude 3.5 Haiku"},
		{ID: "claude-3-opus-20240229", Name: "claude-3-opus", Provider: c.name, Description: "Claude 3 Opus"},
	}, nil
}
