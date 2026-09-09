package google

import "encoding/json"

type GenerateContentRequest struct {
	Model             string            `json:"model,omitempty"`
	Contents          []Content         `json:"contents"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
}

type CountTokensRequest struct {
	GenerateContentRequest *GenerateContentRequest `json:"generateContentRequest"`
}

type CountTokensResponse struct {
	TotalTokens int `json:"totalTokens"`
}

type ErrorResponse struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

type Content struct {
	Role  string `json:"role,omitempty"` // "user" or "model"
	Parts []Part `json:"parts"`
}

type Part struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *Blob             `json:"inlineData,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thought_signature,omitempty"`
}

func (p *Part) UnmarshalJSON(data []byte) error {
	type Alias Part
	var aux struct {
		Alias
		CamelThoughtSignature string `json:"thoughtSignature,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*p = Part(aux.Alias)
	if p.ThoughtSignature == "" && aux.CamelThoughtSignature != "" {
		p.ThoughtSignature = aux.CamelThoughtSignature
	}
	return nil
}

type Blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

type FunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type FunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	Parts    []Part         `json:"parts,omitempty"`
}

type Tool struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type FunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type GenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type GenerateContentResponse struct {
	Candidates    []Candidate    `json:"candidates"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
}

type Candidate struct {
	Content      Content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
	Index        int     `json:"index,omitempty"`
}

type UsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type ModelListResponse struct {
	Models []ModelItem `json:"models"`
}

type ModelItem struct {
	Name                       string   `json:"name"` // e.g. "models/gemini-2.5-pro"
	DisplayName                string   `json:"displayName"`
	Description                string   `json:"description"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}
