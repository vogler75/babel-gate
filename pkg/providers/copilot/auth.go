package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultClientID is the official VS Code GitHub Copilot OAuth client ID
	DefaultClientID = "Iv1.b507a08c87ecfe98"

	DeviceCodeURL = "https://github.com/login/device/code"
	OAuthTokenURL = "https://github.com/login/oauth/access_token"
	CopilotTokenURL = "https://api.github.com/copilot_internal/v2/token"

	DefaultEditorVersion       = "vscode/1.96.2"
	DefaultEditorPluginVersion = "copilot-chat/0.12.0"
	DefaultUserAgent           = "GitHubCopilotChat/0.12.0"
	DefaultIntegrationID       = "vscode-chat"
)

// DeviceCodeResponse represents the response from initiating the device flow.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// OAuthTokenResponse represents the response when polling for an access token.
type OAuthTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	ErrorURI         string `json:"error_uri,omitempty"`
}

// CopilotSessionToken represents the token returned by copilot_internal/v2/token.
type CopilotSessionToken struct {
	Token     string            `json:"token"`
	ExpiresAt int64             `json:"expires_at"`
	Endpoints map[string]string `json:"endpoints"`
}

// RequestDeviceCode initiates the RFC 8628 GitHub device authorization flow.
func RequestDeviceCode(ctx context.Context, client *http.Client) (*DeviceCodeResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}

	payload := map[string]string{
		"client_id": DefaultClientID,
		"scope":     "read:user",
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal device code request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DeviceCodeURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create device code request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send device code request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read device code response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var dcr DeviceCodeResponse
	if err := json.Unmarshal(respBody, &dcr); err != nil {
		return nil, fmt.Errorf("unmarshal device code response: %w", err)
	}

	if dcr.Interval <= 0 {
		dcr.Interval = 5
	}
	return &dcr, nil
}

// PollDeviceToken polls for the access token until authorized, expired, or context cancelled.
func PollDeviceToken(ctx context.Context, client *http.Client, deviceCode string, intervalSeconds int) (*OAuthTokenResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if intervalSeconds <= 0 {
		intervalSeconds = 5
	}

	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			payload := map[string]string{
				"client_id":   DefaultClientID,
				"device_code": deviceCode,
				"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
			}
			bodyBytes, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("marshal poll request: %w", err)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, OAuthTokenURL, bytes.NewReader(bodyBytes))
			if err != nil {
				return nil, fmt.Errorf("create poll request: %w", err)
			}
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				return nil, fmt.Errorf("poll access token: %w", err)
			}

			respBody, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("read poll response: %w", err)
			}

			var otr OAuthTokenResponse
			if err := json.Unmarshal(respBody, &otr); err != nil {
				return nil, fmt.Errorf("unmarshal poll response: %w", err)
			}

			if otr.AccessToken != "" {
				return &otr, nil
			}

			switch otr.Error {
			case "authorization_pending":
				// Waiting for user to complete login in browser
				continue
			case "slow_down":
				// GitHub requests longer interval
				intervalSeconds += 5
				ticker.Reset(time.Duration(intervalSeconds) * time.Second)
			case "expired_token":
				return nil, fmt.Errorf("device code expired; please try again")
			case "access_denied":
				return nil, fmt.Errorf("authorization was denied by user")
			default:
				if otr.Error != "" {
					return nil, fmt.Errorf("oauth error: %s (%s)", otr.Error, otr.ErrorDescription)
				}
			}
		}
	}
}

// GetAuthenticatedUser fetches the GitHub username for the given access token.
func GetAuthenticatedUser(ctx context.Context, client *http.Client, token string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("User-Agent", DefaultUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	var userObj struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&userObj); err != nil {
		return "", err
	}
	return userObj.Login, nil
}

// GetTokenFilePath returns the default path where Copilot tokens are saved.
func GetTokenFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "github-copilot", "hosts.json")
	}
	return filepath.Join(home, ".config", "github-copilot", "hosts.json")
}

// SaveTokenToDisk saves the token to ~/.config/github-copilot/hosts.json.
func SaveTokenToDisk(token, username string) error {
	hostsPath := GetTokenFilePath()
	configDir := filepath.Dir(hostsPath)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("create config dir %s: %w", configDir, err)
	}

	hostsData := make(map[string]map[string]string)
	if data, err := os.ReadFile(hostsPath); err == nil {
		_ = json.Unmarshal(data, &hostsData)
	}

	if hostsData["github.com"] == nil {
		hostsData["github.com"] = make(map[string]string)
	}
	hostsData["github.com"]["oauth_token"] = token
	if username != "" {
		hostsData["github.com"]["user"] = username
	}

	outBytes, err := json.MarshalIndent(hostsData, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hosts.json: %w", err)
	}

	if err := os.WriteFile(hostsPath, outBytes, 0600); err != nil {
		return fmt.Errorf("write %s: %w", hostsPath, err)
	}
	return nil
}

func findTokenInJSONApps(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var apps map[string]struct {
		OAuthToken string `json:"oauth_token"`
	}
	if err := json.Unmarshal(data, &apps); err == nil {
		for _, app := range apps {
			if app.OAuthToken != "" {
				return app.OAuthToken
			}
		}
	}
	return ""
}

func findTokenInJSONHosts(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var hosts map[string]struct {
		OAuthToken string `json:"oauth_token"`
	}
	if err := json.Unmarshal(data, &hosts); err == nil {
		if h, ok := hosts["github.com"]; ok && h.OAuthToken != "" {
			return h.OAuthToken
		}
		for _, h := range hosts {
			if h.OAuthToken != "" {
				return h.OAuthToken
			}
		}
	}
	return ""
}

func findTokenInYAMLHosts(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var ghHosts map[string]struct {
		OAuthToken string `yaml:"oauth_token"`
	}
	if err := yaml.Unmarshal(data, &ghHosts); err == nil {
		if h, ok := ghHosts["github.com"]; ok && h.OAuthToken != "" {
			return h.OAuthToken
		}
		for _, h := range ghHosts {
			if h.OAuthToken != "" {
				return h.OAuthToken
			}
		}
	}
	return ""
}

// ResolveGitHubToken attempts to locate a GitHub / Copilot token from multiple sources:
// 1. Explicit token argument
// 2. COPILOT_API_KEY, GITHUB_TOKEN, GH_TOKEN env vars
// 3. github-copilot hosts.json (~/.config, %LOCALAPPDATA%, %APPDATA%, $XDG_CONFIG_HOME)
// 4. github-copilot apps.json (~/.config, %LOCALAPPDATA%, %APPDATA%, $XDG_CONFIG_HOME)
// 5. GitHub CLI hosts.yml (GH_CONFIG_DIR, %APPDATA%/GitHub CLI, %LOCALAPPDATA%/GitHub CLI, ~/.config/gh)
func ResolveGitHubToken(explicitToken string) string {
	if explicitToken != "" && !strings.HasPrefix(explicitToken, "${") {
		return explicitToken
	}

	for _, env := range []string{"COPILOT_API_KEY", "GITHUB_TOKEN", "GH_TOKEN"} {
		if val := os.Getenv(env); val != "" {
			return val
		}
	}

	home, _ := os.UserHomeDir()
	appData := os.Getenv("APPDATA")
	localAppData := os.Getenv("LOCALAPPDATA")
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")

	// 1. Check github-copilot hosts.json (official Copilot CLI / extension storage)
	var hostsPaths []string
	if xdgConfig != "" {
		hostsPaths = append(hostsPaths, filepath.Join(xdgConfig, "github-copilot", "hosts.json"))
	}
	if home != "" {
		hostsPaths = append(hostsPaths, filepath.Join(home, ".config", "github-copilot", "hosts.json"))
	}
	if localAppData != "" {
		hostsPaths = append(hostsPaths, filepath.Join(localAppData, "github-copilot", "hosts.json"))
	}
	if appData != "" {
		hostsPaths = append(hostsPaths, filepath.Join(appData, "github-copilot", "hosts.json"))
	}
	for _, p := range hostsPaths {
		if token := findTokenInJSONHosts(p); token != "" {
			return token
		}
	}

	// 2. Check github-copilot apps.json
	var appsPaths []string
	if xdgConfig != "" {
		appsPaths = append(appsPaths, filepath.Join(xdgConfig, "github-copilot", "apps.json"))
	}
	if home != "" {
		appsPaths = append(appsPaths, filepath.Join(home, ".config", "github-copilot", "apps.json"))
	}
	if localAppData != "" {
		appsPaths = append(appsPaths, filepath.Join(localAppData, "github-copilot", "apps.json"))
	}
	if appData != "" {
		appsPaths = append(appsPaths, filepath.Join(appData, "github-copilot", "apps.json"))
	}
	for _, p := range appsPaths {
		if token := findTokenInJSONApps(p); token != "" {
			return token
		}
	}

	// 3. Check GitHub CLI hosts.yml
	var ghPaths []string
	if ghConfig := os.Getenv("GH_CONFIG_DIR"); ghConfig != "" {
		ghPaths = append(ghPaths, filepath.Join(ghConfig, "hosts.yml"))
	}
	if appData != "" {
		ghPaths = append(ghPaths, filepath.Join(appData, "GitHub CLI", "hosts.yml"))
	}
	if localAppData != "" {
		ghPaths = append(ghPaths, filepath.Join(localAppData, "GitHub CLI", "hosts.yml"))
	}
	if xdgConfig != "" {
		ghPaths = append(ghPaths, filepath.Join(xdgConfig, "gh", "hosts.yml"))
	}
	if home != "" {
		ghPaths = append(ghPaths, filepath.Join(home, ".config", "gh", "hosts.yml"))
	}
	for _, p := range ghPaths {
		if token := findTokenInYAMLHosts(p); token != "" {
			return token
		}
	}

	return ""
}

// ExchangeCopilotToken exchanges a GitHub OAuth token for a short-lived Copilot session token.
func ExchangeCopilotToken(ctx context.Context, client *http.Client, githubToken string) (*CopilotSessionToken, error) {
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CopilotTokenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create copilot token exchange request: %w", err)
	}

	// GitHub accepts both "token <token>" and "Bearer <token>"
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", DefaultEditorVersion)
	req.Header.Set("Editor-Plugin-Version", DefaultEditorPluginVersion)
	req.Header.Set("User-Agent", DefaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read copilot token exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot token exchange error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var sessionToken CopilotSessionToken
	if err := json.Unmarshal(respBody, &sessionToken); err != nil {
		return nil, fmt.Errorf("unmarshal copilot session token: %w", err)
	}

	if sessionToken.Token == "" {
		return nil, fmt.Errorf("empty session token received from github copilot")
	}

	return &sessionToken, nil
}
