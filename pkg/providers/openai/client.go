package openai

import (
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
		baseURL = "https://api.openai.com/v1"
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

func (c *Client) Name() string     { return c.name }
func (c *Client) Type() string     { return "openai" }
func (c *Client) Endpoint() string { return c.baseURL }
func (c *Client) BaseURL() string  { return c.baseURL }

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
	requestCopy := *req
	req = &requestCopy
	req.Stream = false
	openAIReq, err := ToOpenAIRequest(req)
	if err != nil {
		return nil, fmt.Errorf("transform to openai request: %w", err)
	}

	bodyBytes, err := json.Marshal(openAIReq)
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	apiKey := c.getAPIKey(req)
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

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
		return nil, fmt.Errorf("openai api error status %d: %s", resp.StatusCode, string(respBody))
	}

	var openAIResp ChatCompletionResponse
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return nil, fmt.Errorf("unmarshal openai response: %w", err)
	}

	return openAIReq.RestoreResponse(FromOpenAIResponse(&openAIResp))
}

func (c *Client) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	requestCopy := *req
	req = &requestCopy
	req.Stream = true
	openAIReq, err := ToOpenAIRequest(req)
	if err != nil {
		return nil, fmt.Errorf("transform to openai stream request: %w", err)
	}

	bodyBytes, err := json.Marshal(openAIReq)
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create http stream request: %w", err)
	}

	apiKey := c.getAPIKey(req)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai stream api error %d: %s", resp.StatusCode, string(body))
	}

	eventChan := ReadStream(ctx, resp.Body)

	return openAIReq.RestoreStream(ctx, eventChan), nil
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
		return nil, err
	}
	apiKey := c.getAPIKey(nil)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http execute request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai api error status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res ModelListResponse
	if err := json.Unmarshal(bodyBytes, &res); err == nil && len(res.Data) > 0 {
		var models []providers.ModelInfo
		for _, m := range res.Data {
			models = append(models, providers.ModelInfo{
				ID:       m.ID,
				Name:     m.ID,
				Provider: c.name,
			})
		}
		return models, nil
	}

	// Try generic map with "data" or "models" array
	var genericMap map[string]any
	if err := json.Unmarshal(bodyBytes, &genericMap); err == nil {
		// Enterprise gateway catalog structure: {"catalog": [{"tool": "...", "base_path": "...", "models": [...]}]}
		if catalogArr, ok := genericMap["catalog"].([]any); ok {
			var models []providers.ModelInfo
			seen := make(map[string]bool)
			for _, catItem := range catalogArr {
				if catMap, ok := catItem.(map[string]any); ok {
					tool, _ := catMap["tool"].(string)
					basePath, _ := catMap["base_path"].(string)
					toolLower := strings.ToLower(tool)
					if strings.Contains(basePath, "chat/completions") || strings.Contains(toolLower, "openai") {
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
			// Fallback: if no OpenAI-specific entry was found, include all catalog models
			if len(models) == 0 {
				for _, catItem := range catalogArr {
					if catMap, ok := catItem.(map[string]any); ok {
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

		for _, key := range []string{"data", "models"} {
			if arr, ok := genericMap[key].([]any); ok {
				var models []providers.ModelInfo
				for _, item := range arr {
					if itemMap, ok := item.(map[string]any); ok {
						if id, ok := itemMap["id"].(string); ok && id != "" {
							models = append(models, providers.ModelInfo{ID: id, Name: id, Provider: c.name})
						} else if name, ok := itemMap["name"].(string); ok && name != "" {
							models = append(models, providers.ModelInfo{ID: name, Name: name, Provider: c.name})
						}
					} else if str, ok := item.(string); ok && str != "" {
						models = append(models, providers.ModelInfo{ID: str, Name: str, Provider: c.name})
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
