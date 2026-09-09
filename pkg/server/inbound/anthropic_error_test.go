package inbound

import (
	"net/http"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/providers"
)

func TestAnthropicErrorClassifiesUpstreamInvalidRequest(t *testing.T) {
	status, errorType, message := anthropicError(&providers.APIError{
		Provider:   "google",
		Operation:  "stream",
		StatusCode: http.StatusBadRequest,
		Status:     "INVALID_ARGUMENT",
		Message:    "The input token count exceeds the maximum allowed.",
	})
	if status != http.StatusBadRequest || errorType != "invalid_request_error" {
		t.Fatalf("unexpected classification: status=%d type=%q", status, errorType)
	}
	if !strings.Contains(message, "google") || !strings.Contains(message, "token count") {
		t.Fatalf("upstream context was lost: %q", message)
	}
}

func TestAnthropicErrorKeepsUnknownFailuresAsGatewayErrors(t *testing.T) {
	status, errorType, _ := anthropicError(assertionError("network failed"))
	if status != http.StatusBadGateway || errorType != "api_error" {
		t.Fatalf("unexpected classification: status=%d type=%q", status, errorType)
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }
