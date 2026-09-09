package providers

import (
	"context"
	"fmt"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// ModelInfo describes a model supported by a provider.
type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Description string `json:"description,omitempty"`
}

// Provider represents an upstream LLM service driver.
type Provider interface {
	Name() string
	Type() string
	Endpoint() string
	Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error)
	Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// TokenCounter is implemented by providers that can count an exact request
// using the tokenizer of the routed upstream model.
type TokenCounter interface {
	CountTokens(ctx context.Context, req *canonical.CanonicalRequest) (int, error)
}

// APIError preserves a provider's structured HTTP error so inbound protocol
// handlers can translate it without parsing a formatted error string.
type APIError struct {
	Provider   string
	Operation  string
	StatusCode int
	Code       int
	Status     string
	Message    string
}

func (e *APIError) Error() string {
	prefix := e.Provider
	if e.Operation != "" {
		prefix += " " + e.Operation
	}
	detail := e.Status
	if detail == "" && e.Code != 0 {
		detail = fmt.Sprintf("code %d", e.Code)
	}
	if detail != "" {
		return fmt.Sprintf("%s API error %d (%s): %s", prefix, e.StatusCode, detail, e.Message)
	}
	return fmt.Sprintf("%s API error %d: %s", prefix, e.StatusCode, e.Message)
}
