package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/vogler75/babel-gate/pkg/providers/copilot"
)

// ServerConfig defines HTTP server settings.
type ServerConfig struct {
	Port           int      `yaml:"port"`
	APIKey         string   `yaml:"api_key"` // Optional router key
	CORSOrigins    []string `yaml:"cors_origins"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

// ProviderConfig defines configuration for an upstream LLM provider.
type ProviderConfig struct {
	Type          string   `yaml:"type"` // "openai", "anthropic", "google", "copilot"
	APIKey        string   `yaml:"api_key"`
	BaseURL       string   `yaml:"base_url"`
	EnabledModels []string `yaml:"enabled_models"`
	DefaultModel  string   `yaml:"default_model"`
	Priority      int      `yaml:"priority"` // 1 is highest priority, 2 is second, etc. Defaults to 100 if unset.
}

// RoutingConfig defines model aliasing and routing rules.
type RoutingConfig struct {
	Default   string              `yaml:"default"`
	Routes    map[string]string   `yaml:"routes"`
	Fallbacks map[string][]string `yaml:"fallbacks"`
}

// Config is the top-level configuration structure.
type Config struct {
	Server    ServerConfig              `yaml:"server"`
	Providers map[string]ProviderConfig `yaml:"providers"`
	Routing   RoutingConfig             `yaml:"routing"`
}

var envRegex = regexp.MustCompile(`\$\{([a-zA-Z_0-9]+)\}|\$([a-zA-Z_0-9]+)`)

// expandEnv replaces ${VAR} or $VAR with environment variable values.
func expandEnv(content string) string {
	return envRegex.ReplaceAllStringFunc(content, func(m string) string {
		var varName string
		if strings.HasPrefix(m, "${") && strings.HasSuffix(m, "}") {
			varName = m[2 : len(m)-1]
		} else if strings.HasPrefix(m, "$") {
			varName = m[1:]
		}
		if val, ok := os.LookupEnv(varName); ok {
			return val
		}
		return m
	})
}

func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "="); idx != -1 {
			k := strings.TrimSpace(line[:idx])
			v := strings.TrimSpace(line[idx+1:])
			v = strings.Trim(v, `"'`)
			_ = os.Setenv(k, v)
		}
	}
}

// Load loads the configuration from a file. If path is empty, it returns default config initialized from environment.
func Load(path string) (*Config, error) {
	loadDotEnv()
	cfg := &Config{
		Server: ServerConfig{
			Port:           8080,
			TimeoutSeconds: 120,
			CORSOrigins:    []string{"*"},
		},
		Providers: make(map[string]ProviderConfig),
		Routing: RoutingConfig{
			Routes:    make(map[string]string),
			Fallbacks: make(map[string][]string),
		},
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config file %s: %w", path, err)
		}
		expanded := expandEnv(string(data))
		if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
			return nil, fmt.Errorf("parsing yaml config: %w", err)
		}
	}

	// Auto-detect environment variables for missing providers
	autoPopulateFromEnv(cfg)

	// Validate and set defaults
	if portStr := os.Getenv("PORT"); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			cfg.Server.Port = p
		}
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.TimeoutSeconds == 0 {
		cfg.Server.TimeoutSeconds = 120
	}

	return cfg, nil
}

func autoPopulateFromEnv(cfg *Config) {
	// If providers are already declared in config, do not inject unconfigured cloud providers
	if len(cfg.Providers) > 0 {
		return
	}

	cfg.Providers = make(map[string]ProviderConfig)

	// OpenAI
	if openaiKey := os.Getenv("OPENAI_API_KEY"); openaiKey != "" {
		if _, exists := cfg.Providers["openai"]; !exists {
			cfg.Providers["openai"] = ProviderConfig{
				Type:   "openai",
				APIKey: openaiKey,
			}
		}
	}

	// Anthropic
	if anthropicKey := os.Getenv("ANTHROPIC_API_KEY"); anthropicKey != "" {
		if _, exists := cfg.Providers["anthropic"]; !exists {
			cfg.Providers["anthropic"] = ProviderConfig{
				Type:   "anthropic",
				APIKey: anthropicKey,
			}
		}
	}

	// Google Gemini
	googleKey := os.Getenv("GEMINI_API_KEY")
	if googleKey == "" {
		googleKey = os.Getenv("GOOGLE_API_KEY")
	}
	if googleKey != "" {
		if _, exists := cfg.Providers["google"]; !exists {
			cfg.Providers["google"] = ProviderConfig{
				Type:   "google",
				APIKey: googleKey,
			}
		}
	}

	// GitHub Copilot
	if copilotToken := copilot.ResolveGitHubToken(""); copilotToken != "" {
		if _, exists := cfg.Providers["copilot"]; !exists {
			cfg.Providers["copilot"] = ProviderConfig{
				Type:   "copilot",
				APIKey: copilotToken,
			}
		}
	}
}
