package copilot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
	"github.com/vogler75/babel-gate/pkg/providers"
	"github.com/vogler75/babel-gate/pkg/providers/openai"
)

// Client implements providers.Provider for GitHub Copilot.
type Client struct {
	name            string
	rawGitHubToken  string
	explicitBaseURL string
	httpClient      *http.Client
	enabledModels   []string

	mu             sync.RWMutex
	sessionToken   string
	sessionExpiry  time.Time
	dynamicBaseURL string
}

// NewClient creates a new GitHub Copilot provider client.
func NewClient(name, apiKey, baseURL string, enabledModels []string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL != "" {
		baseURL = strings.TrimSuffix(baseURL, "/")
	}

	return &Client{
		name:            name,
		rawGitHubToken:  apiKey,
		explicitBaseURL: baseURL,
		httpClient:      httpClient,
		enabledModels:   enabledModels,
	}
}

func (c *Client) Name() string { return c.name }
func (c *Client) Type() string { return "copilot" }

// ensureSessionToken returns a valid Copilot session token and base URL, refreshing if necessary.
func (c *Client) ensureSessionToken(ctx context.Context, clientAuthToken string) (string, string, error) {
	// 1. Fast path: check read lock if cached token is still valid (with 2m buffer)
	c.mu.RLock()
	if c.sessionToken != "" && time.Now().Before(c.sessionExpiry.Add(-2*time.Minute)) {
		token := c.sessionToken
		bURL := c.getBaseURL()
		c.mu.RUnlock()
		return token, bURL, nil
	}
	c.mu.RUnlock()

	// 2. Slow path: acquire write lock to exchange or refresh
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double check after acquiring write lock
	if c.sessionToken != "" && time.Now().Before(c.sessionExpiry.Add(-2*time.Minute)) {
		return c.sessionToken, c.getBaseURL(), nil
	}

	// Resolve the underlying GitHub token
	ghToken := c.rawGitHubToken
	if clientAuthToken != "" && !strings.HasPrefix(clientAuthToken, "dummy") && !strings.HasPrefix(clientAuthToken, "test") {
		// If the caller passed a session token directly (starts with tid=), use it
		if strings.Contains(clientAuthToken, "tid=") {
			c.sessionToken = clientAuthToken
			c.sessionExpiry = time.Now().Add(30 * time.Minute)
			return c.sessionToken, c.getBaseURL(), nil
		}
		ghToken = clientAuthToken
	}

	ghToken = ResolveGitHubToken(ghToken)
	if ghToken == "" {
		return "", "", fmt.Errorf("no GitHub Copilot token found. Run router with '-copilot-login' or visit http://localhost:8080/setup to connect")
	}

	// If token itself is already a Copilot session token (contains tid=)
	if strings.Contains(ghToken, "tid=") {
		c.sessionToken = ghToken
		c.sessionExpiry = time.Now().Add(30 * time.Minute)
		return c.sessionToken, c.getBaseURL(), nil
	}

	// Exchange GitHub OAuth/PAT for Copilot session token
	session, err := ExchangeCopilotToken(ctx, c.httpClient, ghToken)
	if err != nil {
		return "", "", fmt.Errorf("exchange copilot session token: %w", err)
	}

	c.sessionToken = session.Token
	if session.ExpiresAt > 0 {
		c.sessionExpiry = time.Unix(session.ExpiresAt, 0)
	} else {
		c.sessionExpiry = time.Now().Add(25 * time.Minute)
	}

	if apiEndpoint, ok := session.Endpoints["api"]; ok && apiEndpoint != "" {
		c.dynamicBaseURL = strings.TrimSuffix(apiEndpoint, "/")
	}

	return c.sessionToken, c.getBaseURL(), nil
}

func (c *Client) getBaseURL() string {
	if c.explicitBaseURL != "" {
		return c.explicitBaseURL
	}
	if c.dynamicBaseURL != "" {
		return c.dynamicBaseURL
	}
	return "https://api.individual.githubcopilot.com"
}

// setCopilotHeaders sets the required headers for Copilot chat completions.
func (c *Client) setCopilotHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Editor-Version", DefaultEditorVersion)
	req.Header.Set("Editor-Plugin-Version", DefaultEditorPluginVersion)
	req.Header.Set("User-Agent", DefaultUserAgent)
	req.Header.Set("Copilot-Integration-Id", DefaultIntegrationID)
	req.Header.Set("Openai-Intent", "conversation-panel")
}

// Execute executes a non-streaming canonical request against GitHub Copilot.
func (c *Client) Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error) {
	req.Stream = false
	token, baseURL, err := c.ensureSessionToken(ctx, req.AuthToken)
	if err != nil {
		return nil, err
	}

	openAIReq, err := openai.ToOpenAIRequest(req)
	if err != nil {
		return nil, fmt.Errorf("transform canonical request: %w", err)
	}

	bodyBytes, err := json.Marshal(openAIReq)
	if err != nil {
		return nil, fmt.Errorf("marshal copilot request: %w", err)
	}

	url := baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create copilot http request: %w", err)
	}
	c.setCopilotHeaders(httpReq, token)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute copilot http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read copilot response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot api error status %d: %s", resp.StatusCode, string(respBody))
	}

	var openAIResp openai.ChatCompletionResponse
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return nil, fmt.Errorf("unmarshal copilot response: %w", err)
	}

	return openai.FromOpenAIResponse(&openAIResp)
}

// Stream executes a streaming canonical request against GitHub Copilot.
func (c *Client) Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error) {
	req.Stream = true
	token, baseURL, err := c.ensureSessionToken(ctx, req.AuthToken)
	if err != nil {
		return nil, err
	}

	openAIReq, err := openai.ToOpenAIRequest(req)
	if err != nil {
		return nil, fmt.Errorf("transform canonical request: %w", err)
	}

	bodyBytes, err := json.Marshal(openAIReq)
	if err != nil {
		return nil, fmt.Errorf("marshal copilot request: %w", err)
	}

	url := baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create copilot http request: %w", err)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	c.setCopilotHeaders(httpReq, token)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute copilot stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("copilot stream api error status %d: %s", resp.StatusCode, string(errBytes))
	}

	outCh := make(chan canonical.CanonicalEvent, 64)

	go func() {
		defer resp.Body.Close()
		defer close(outCh)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					outCh <- canonical.CanonicalEvent{Type: canonical.EventMessageDone}
					return
				}

				chunk, err := openai.UnmarshalStreamChunk([]byte(data))
				if err != nil {
					continue
				}

				events := openai.ParseOpenAIStreamEvent(chunk)
				for _, ev := range events {
					select {
					case <-ctx.Done():
						outCh <- canonical.CanonicalEvent{Type: canonical.EventError, Error: ctx.Err()}
						return
					case outCh <- ev:
					}
				}
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			outCh <- canonical.CanonicalEvent{Type: canonical.EventError, Error: err}
		}
	}()

	return outCh, nil
}

// ListModels queries GitHub Copilot's /models endpoint and returns available chat models.
func (c *Client) ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	token, baseURL, err := c.ensureSessionToken(ctx, "")
	if err != nil {
		// Fallback to standard Copilot models if auth not yet configured
		return c.fallbackModels(), nil
	}

	url := baseURL + "/models"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return c.fallbackModels(), nil
	}
	c.setCopilotHeaders(httpReq, token)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return c.fallbackModels(), nil
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return c.fallbackModels(), nil
	}

	type CopilotModelEntry struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Capabilities struct {
			Type string `json:"type"`
		} `json:"capabilities"`
		Policy struct {
			State string `json:"state"`
		} `json:"policy"`
	}

	var res struct {
		Data []CopilotModelEntry `json:"data"`
	}

	if err := json.Unmarshal(bodyBytes, &res); err != nil || len(res.Data) == 0 {
		return c.fallbackModels(), nil
	}

	var models []providers.ModelInfo
	seen := make(map[string]bool)

	for _, m := range res.Data {
		// Skip disabled models or pure embedding models
		if m.Policy.State == "disabled" || m.Capabilities.Type == "embeddings" {
			continue
		}
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true

		dispName := m.Name
		if dispName == "" {
			dispName = m.ID
		}

		models = append(models, providers.ModelInfo{
			ID:          m.ID,
			Name:        dispName,
			Provider:    c.name,
			Description: fmt.Sprintf("GitHub Copilot model (%s)", m.ID),
		})
	}

	// If enabled models is specified, filter to those
	if len(c.enabledModels) > 0 {
		enabledMap := make(map[string]bool)
		for _, m := range c.enabledModels {
			enabledMap[m] = true
		}
		var filtered []providers.ModelInfo
		for _, m := range models {
			if enabledMap[m.ID] {
				filtered = append(filtered, m)
			}
		}
		return filtered, nil
	}

	if len(models) == 0 {
		return c.fallbackModels(), nil
	}

	return models, nil
}

func (c *Client) fallbackModels() []providers.ModelInfo {
	defaultModels := []string{
		"gpt-4o",
		"gpt-4o-mini",
		"claude-3.5-sonnet",
		"o1",
		"o3-mini",
	}
	if len(c.enabledModels) > 0 {
		defaultModels = c.enabledModels
	}

	var models []providers.ModelInfo
	for _, id := range defaultModels {
		models = append(models, providers.ModelInfo{
			ID:          id,
			Name:        id,
			Provider:    c.name,
			Description: "GitHub Copilot model",
		})
	}
	return models
}
