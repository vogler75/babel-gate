package openai

import (
	"context"
	"github.com/vogler75/babel-gate/pkg/canonical"
)

func (r *ChatCompletionRequest) RestoreResponse(resp *canonical.CanonicalResponse, err error) (*canonical.CanonicalResponse, error) {
	return r.names.RestoreResponse(resp, err)
}
func (r *ChatCompletionRequest) RestoreStream(ctx context.Context, ch <-chan canonical.CanonicalEvent) <-chan canonical.CanonicalEvent {
	return r.names.RestoreStream(ctx, ch)
}
