package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandEnv(t *testing.T) {
	os.Setenv("TEST_KEY", "secret_123")
	defer os.Unsetenv("TEST_KEY")

	input := "api_key: ${TEST_KEY}\nother: $TEST_KEY\nplain: unchanged"
	got := expandEnv(input)
	expected := "api_key: secret_123\nother: secret_123\nplain: unchanged"
	if got != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, got)
	}
}

func TestProviderEnabledDefaultsToTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("providers:\n  openai:\n    type: openai\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["openai"].IsEnabled() {
		t.Fatal("provider without enabled field should remain enabled")
	}
}

func TestUpdateProviderEnabledPreservesEnvironmentReference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := "# keep this comment\nproviders:\n  openai:\n    type: openai\n    api_key: ${OPENAI_API_KEY}\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := UpdateProviderEnabled(path, "openai", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "enabled: false") || !strings.Contains(text, "${OPENAI_API_KEY}") || !strings.Contains(text, "# keep this comment") {
		t.Fatalf("unexpected updated config:\n%s", text)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("expected mode 0640, got %o", info.Mode().Perm())
	}
}

func TestLoadRouting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := "providers: {}\nrouting:\n  default: openai/gpt-4o\n  routes:\n    fast: openai/gpt-4o-mini\n  fallbacks:\n    fast: [openai/gpt-4o]\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	routing, err := LoadRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if routing.Default != "openai/gpt-4o" || routing.Routes["fast"] != "openai/gpt-4o-mini" || len(routing.Fallbacks["fast"]) != 1 {
		t.Fatalf("unexpected routing: %+v", routing)
	}
}

func TestAutoPopulateFromEnv(t *testing.T) {
	os.Setenv("OPENAI_API_KEY", "sk-openai")
	os.Setenv("ANTHROPIC_API_KEY", "sk-anthropic")
	os.Setenv("GEMINI_API_KEY", "sk-gemini")
	defer func() {
		os.Unsetenv("OPENAI_API_KEY")
		os.Unsetenv("ANTHROPIC_API_KEY")
		os.Unsetenv("GEMINI_API_KEY")
	}()

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Providers["openai"].APIKey != "sk-openai" {
		t.Errorf("expected openai key sk-openai, got %s", cfg.Providers["openai"].APIKey)
	}
	if cfg.Providers["anthropic"].APIKey != "sk-anthropic" {
		t.Errorf("expected anthropic key sk-anthropic, got %s", cfg.Providers["anthropic"].APIKey)
	}
	if cfg.Providers["google"].APIKey != "sk-gemini" {
		t.Errorf("expected google key sk-gemini, got %s", cfg.Providers["google"].APIKey)
	}
}
