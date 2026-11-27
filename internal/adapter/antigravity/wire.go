package antigravity

import "encoding/json"

// GenerateContentRequest contains the Gemini input and generation options.
type GenerateContentRequest struct {
	SystemInstruction *content         `json:"systemInstruction,omitempty"`
	Contents          []content        `json:"contents"`
	Tools             []tool           `json:"tools,omitempty"`
	ToolConfig        *toolConfig      `json:"toolConfig,omitempty"`
	GenerationConfig  generationConfig `json:"generationConfig,omitzero"`
}

// CodeAssistEnvelope wraps a Gemini request in the Code Assist wire format.
type CodeAssistEnvelope struct {
	Model       string            `json:"model"`
	Project     string            `json:"project"`
	UserAgent   string            `json:"userAgent"`
	RequestType string            `json:"requestType"`
	RequestID   string            `json:"requestId"`
	Request     CodeAssistRequest `json:"request"`
}

// CodeAssistRequest adds the Code Assist session identifier to the Gemini body.
type CodeAssistRequest struct {
	GenerateContentRequest

	SessionID string `json:"sessionId"`
}

type content struct {
	Role  messageRole `json:"role,omitempty"`
	Parts []part      `json:"parts"`
}

type part struct {
	Text             *string           `json:"text,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
}

type inlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"` // The caller defines the argument schema.
}

type functionResponse struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response functionOutput `json:"response"`
}

type functionOutput struct {
	Output string `json:"output"`
}

type tool struct {
	FunctionDeclarations []declaration `json:"functionDeclarations"`
}

type declaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parametersJsonSchema,omitempty"` // Caller-defined JSON Schema.
}

type toolConfig struct {
	Calling callingConfig `json:"functionCallingConfig"`
}

type callingConfig struct {
	Mode  string   `json:"mode"`
	Names []string `json:"allowedFunctionNames,omitempty"`
}

type generationConfig struct {
	MaxOutputTokens  *int            `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseJsonSchema,omitempty"` // Caller-defined JSON Schema.
	Thinking         *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	Level           string `json:"thinkingLevel,omitempty"`
	Budget          int    `json:"thinkingBudget,omitempty"`
	IncludeThoughts bool   `json:"includeThoughts"`
}
