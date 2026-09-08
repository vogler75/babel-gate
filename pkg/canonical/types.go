package canonical

// Standard roles
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// PartType identifies the variant of ContentPart.
type PartType string

const (
	PartText       PartType = "text"
	PartThinking   PartType = "thinking"
	PartImage      PartType = "image"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
)

// ContentPart represents a single piece of content within a Message.
type ContentPart struct {
	Type PartType `json:"type"`

	// For PartText
	Text string `json:"text,omitempty"`

	// For PartThinking
	Thinking string `json:"thinking,omitempty"`

	// For PartImage
	ImageMediaType string `json:"image_media_type,omitempty"`
	ImageData      string `json:"image_data,omitempty"` // base64
	ImageURL       string `json:"image_url,omitempty"`

	// For PartToolCall
	ToolCallID       string `json:"tool_call_id,omitempty"`
	ToolCallName     string `json:"tool_call_name,omitempty"`
	ToolCallArgs     string `json:"tool_call_args,omitempty"` // JSON string
	ThoughtSignature string `json:"thought_signature,omitempty"`

	// For PartToolResult
	ToolResultID      string `json:"tool_result_id,omitempty"`
	ToolResultContent string `json:"tool_result_content,omitempty"`
	ToolResultError   bool   `json:"tool_result_error,omitempty"`
}

// Message represents a canonical conversation turn.
type Message struct {
	Role  string        `json:"role"`
	Parts []ContentPart `json:"parts"`
}

// TextContent returns the concatenated text content of all text parts in the message.
func (m *Message) TextContent() string {
	var s string
	for _, p := range m.Parts {
		if p.Type == PartText {
			s += p.Text
		}
	}
	return s
}

// ToolDeclaration specifies a function tool that an LLM can invoke.
type ToolDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"` // JSON Schema
}

// Parameters defines model generation tuning options.
type Parameters struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	TopK        *int     `json:"top_k,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

// ToolChoice selects automatic, required, disabled, or a named function.
type ToolChoice struct {
	Mode string `json:"mode"`
	Name string `json:"name,omitempty"`
}

// CanonicalRequest is the normalized request passed into the routing and provider layers.
type CanonicalRequest struct {
	ToolChoice *ToolChoice       `json:"tool_choice,omitempty"`
	Model      string            `json:"model"`
	Messages   []Message         `json:"messages"`
	Tools      []ToolDeclaration `json:"tools,omitempty"`
	Params     Parameters        `json:"params,omitempty"`
	Stream     bool              `json:"stream,omitempty"`
	AuthToken  string            `json:"auth_token,omitempty"`
}

// SystemPrompt extracts and concatenates any leading or internal system messages.
func (r *CanonicalRequest) SystemPrompt() string {
	var sys string
	for _, m := range r.Messages {
		if m.Role == RoleSystem {
			if sys != "" {
				sys += "\n\n"
			}
			sys += m.TextContent()
		}
	}
	return sys
}

// NonSystemMessages returns all messages that are not system role.
func (r *CanonicalRequest) NonSystemMessages() []Message {
	var msgs []Message
	for _, m := range r.Messages {
		if m.Role != RoleSystem {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

// Usage captures token counts.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CanonicalResponse is the normalized response returned by providers for non-streaming requests.
type CanonicalResponse struct {
	ID           string  `json:"id"`
	Model        string  `json:"model"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"` // "stop", "tool_calls", "length", etc.
	Usage        Usage   `json:"usage"`
}
