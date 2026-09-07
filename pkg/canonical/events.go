package canonical

// EventType identifies the kind of streaming event.
type EventType string

const (
	EventMessageStart   EventType = "message_start"
	EventThinkingDelta  EventType = "thinking_delta"
	EventTextDelta      EventType = "text_delta"
	EventToolCallStart  EventType = "tool_call_start"
	EventToolCallDelta  EventType = "tool_call_delta"
	EventToolCallDone   EventType = "tool_call_done"
	EventMessageDelta   EventType = "message_delta"
	EventMessageDone    EventType = "message_done"
	EventError          EventType = "error"
)

// CanonicalEvent represents a normalized streaming chunk.
type CanonicalEvent struct {
	Type EventType `json:"type"`

	// Message identifier & model
	MessageID string `json:"message_id,omitempty"`
	Model     string `json:"model,omitempty"`

	// Content blocks
	Index        int    `json:"index,omitempty"`
	Text         string `json:"text,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	ToolCallID       string `json:"tool_call_id,omitempty"`
	ToolCallName     string `json:"tool_call_name,omitempty"`
	ToolCallArgs     string `json:"tool_call_args,omitempty"` // incremental delta for args
	ThoughtSignature string `json:"thought_signature,omitempty"`

	// Completion status & usage
	FinishReason string `json:"finish_reason,omitempty"` // "stop", "tool_calls", "length"
	Usage        *Usage `json:"usage,omitempty"`
	Error        error  `json:"error,omitempty"`
}
