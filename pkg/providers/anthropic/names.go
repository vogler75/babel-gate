package anthropic

import (
	"context"
	"github.com/vogler75/babel-gate/pkg/canonical"
)

func (r *MessageRequest) RestoreResponse(resp *canonical.CanonicalResponse, err error) (*canonical.CanonicalResponse, error) {
	return r.names.RestoreResponse(resp, err)
}
func (r *MessageRequest) RestoreStream(ctx context.Context, ch <-chan canonical.CanonicalEvent) <-chan canonical.CanonicalEvent {
	return r.names.RestoreStream(ctx, ch)
}
