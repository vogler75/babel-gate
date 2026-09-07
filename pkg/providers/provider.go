package providers

import (
	"context"

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
	Execute(ctx context.Context, req *canonical.CanonicalRequest) (*canonical.CanonicalResponse, error)
	Stream(ctx context.Context, req *canonical.CanonicalRequest) (<-chan canonical.CanonicalEvent, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
}
