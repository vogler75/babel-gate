package anthropic

import "github.com/vogler75/babel-gate/pkg/providers/toolnames"

import "encoding/json"

type MessageRequest struct {
	names         *toolnames.Mapping
	ToolChoice    any              `json:"tool_choice,omitempty"`
	Model         string           `json:"model"`
	Messages      []Message        `json:"messages"`
	System        any              `json:"system,omitempty"` // string or []SystemPart
	MaxTokens     int              `json:"max_tokens"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	TopK          *int             `json:"top_k,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Tools         []ToolDefinition `json:"tools,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
}

type Message struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content any    `json:"content"` // string or []ContentBlock
}

type ContentBlock struct {
	Type string `json:"type"` // "text", "image", "tool_use", "tool_result", "thinking", "redacted_thinking"

	// text
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// redacted_thinking
	Data string `json:"data,omitempty"`

	// image
	Source *ImageSource `json:"source,omitempty"`

	// tool_use
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"` // map[string]any or raw json

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"` // string or []ContentBlock
	IsError   bool   `json:"is_error,omitempty"`
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type Alias ContentBlock
	if b.Type == "thinking" {
		m := map[string]any{
			"type":     "thinking",
			"thinking": b.Thinking,
		}
		if b.Signature != "" {
			m["signature"] = b.Signature
		}
		return json.Marshal(m)
	}
	if b.Type == "redacted_thinking" {
		m := map[string]any{
			"type": "redacted_thinking",
			"data": b.Data,
		}
		return json.Marshal(m)
	}
	return json.Marshal((*Alias)(&b))
}

type ImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type MessageResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"` // "end_turn", "tool_use", "max_tokens"
	Usage      Usage          `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Streaming event structures
type StreamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index,omitempty"`
	Message      *MessageInfo  `json:"message,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	Delta        *StreamDelta  `json:"delta,omitempty"`
	Usage        *Usage        `json:"usage,omitempty"`
}

type MessageInfo struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`
	Usage Usage  `json:"usage"`
}

type StreamDelta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}
