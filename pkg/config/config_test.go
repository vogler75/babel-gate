package config

import (
	"os"
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
