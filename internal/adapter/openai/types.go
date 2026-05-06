package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"goodkind.io/clyde/internal/adapter/errcontract"
)

type ChatRequest struct {
	Model                string          `json:"model"`
	Messages             []ChatMessage   `json:"messages"`
	Input                json.RawMessage `json:"input,omitempty"`
	Stream               bool            `json:"stream,omitempty"`
	StreamOptions        *StreamOptions  `json:"stream_options,omitempty"`
	ReasoningEffort      string          `json:"reasoning_effort,omitempty"`
	Reasoning            *Reasoning      `json:"reasoning,omitempty"`
	Tools                []Tool          `json:"tools,omitempty"`
	ToolChoice           json.RawMessage `json:"tool_choice,omitempty"`
	Functions            []Function      `json:"functions,omitempty"`
	FunctionCall         json.RawMessage `json:"function_call,omitempty"`
	N                    int             `json:"n,omitempty"`
	User                 string          `json:"user,omitempty"`
	Temperature          *float64        `json:"temperature,omitempty"`
	TopP                 *float64        `json:"top_p,omitempty"`
	MaxTokens            *int            `json:"max_tokens,omitempty"`
	MaxComplTokens       *int            `json:"max_completion_tokens,omitempty"`
	MaxOutputTokens      *int            `json:"max_output_tokens,omitempty"`
	PresencePenalty      *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty     *float64        `json:"frequency_penalty,omitempty"`
	LogitBias            json.RawMessage `json:"logit_bias,omitempty"`
	Logprobs             *bool           `json:"logprobs,omitempty"`
	TopLogprobs          *int            `json:"top_logprobs,omitempty"`
	Stop                 json.RawMessage `json:"stop,omitempty"`
	Seed                 *int            `json:"seed,omitempty"`
	ResponseFormat       json.RawMessage `json:"response_format,omitempty"`
	Audio                json.RawMessage `json:"audio,omitempty"`
	Modalities           json.RawMessage `json:"modalities,omitempty"`
	ParallelTools        *bool           `json:"parallel_tool_calls,omitempty"`
	Store                *bool           `json:"store,omitempty"`
	Metadata             json.RawMessage `json:"metadata,omitempty"`
	Include              []string        `json:"include,omitempty"`
	ServiceTier          string          `json:"service_tier,omitempty"`
	Text                 json.RawMessage `json:"text,omitempty"`
	Truncation           string          `json:"truncation,omitempty"`
	PromptCacheRetention string          `json:"prompt_cache_retention,omitempty"`
}

type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type Tool struct {
	Type     string             `json:"type"`
	Function ToolFunctionSchema `json:"function"`
}

func (t *Tool) UnmarshalJSON(raw []byte) error {
	type rawTool struct {
		Type        string              `json:"type"`
		Function    *ToolFunctionSchema `json:"function"`
		Name        string              `json:"name"`
		Description string              `json:"description"`
		Parameters  json.RawMessage     `json:"parameters"`
		InputSchema json.RawMessage     `json:"input_schema"`
		Strict      *bool               `json:"strict"`
	}

	var w rawTool
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}

	if w.Function != nil {
		if w.Type != "" && w.Type != "function" {
			return fmt.Errorf("tool has unsupported type %q", w.Type)
		}
		t.Type = "function"
		t.Function = *w.Function
		return nil
	}

	if w.Name == "" {
		return fmt.Errorf("tool missing function schema")
	}
	switch w.Type {
	case "", "function", "custom":
	default:
		return fmt.Errorf("tool has unsupported type %q", w.Type)
	}

	parameters := w.Parameters
	if len(parameters) == 0 {
		parameters = w.InputSchema
	}

	t.Type = "function"
	t.Function = ToolFunctionSchema{
		Name:        w.Name,
		Description: w.Description,
		Parameters:  parameters,
		Strict:      w.Strict,
	}
	return nil
}

type ToolFunctionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ChatMessage struct {
	Role             string              `json:"role"`
	Content          json.RawMessage     `json:"content,omitempty"`
	Name             string              `json:"name,omitempty"`
	ToolCalls        []ToolCall          `json:"tool_calls,omitempty"`
	ToolCallID       string              `json:"tool_call_id,omitempty"`
	Reasoning        string              `json:"reasoning,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	Refusal          string              `json:"refusal,omitempty"`
	Annotations      []MessageAnnotation `json:"annotations,omitempty"`
}

type MessageAnnotation struct {
	Type        string       `json:"type"`
	URLCitation *URLCitation `json:"url_citation,omitempty"`
}

type URLCitation struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

type ToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function,omitzero"`
}

type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ContentPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ImageURL  *ImageURLPart   `json:"image_url,omitempty"`
	Audio     *AudioInputRef  `json:"input_audio,omitempty"`
	Refusal   string          `json:"refusal,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type ImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type AudioInputRef struct {
	Data   string `json:"data"`
	Format string `json:"format,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ChatResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	Choices           []ChatChoice `json:"choices"`
	Usage             *Usage       `json:"usage,omitempty"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
}

type ChatChoice struct {
	Index        int             `json:"index"`
	Message      ChatMessage     `json:"message"`
	Logprobs     *LogprobsResult `json:"logprobs,omitempty"`
	FinishReason string          `json:"finish_reason"`
}

type LogprobsResult struct {
	Content []LogprobToken `json:"content,omitempty"`
}

type LogprobToken struct {
	Token       string       `json:"token"`
	Logprob     float64      `json:"logprob"`
	Bytes       []int        `json:"bytes,omitempty"`
	TopLogprobs []TopLogprob `json:"top_logprobs,omitempty"`
}

type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

type StreamChunk struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	Choices           []StreamChoice `json:"choices"`
	Usage             *Usage         `json:"usage,omitempty"`
	SystemFingerprint string         `json:"system_fingerprint,omitempty"`
}

type StreamChoice struct {
	Index        int             `json:"index"`
	Delta        StreamDelta     `json:"delta"`
	Logprobs     *LogprobsResult `json:"logprobs,omitempty"`
	FinishReason *string         `json:"finish_reason"`
}

type StreamDelta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	Reasoning        string     `json:"reasoning,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	Refusal          string     `json:"refusal,omitempty"`
}

type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	InputTokens         int                  `json:"input_tokens,omitempty"`
	OutputTokens        int                  `json:"output_tokens,omitempty"`
	CacheReadTokens     int                  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens    int                  `json:"cache_write_tokens,omitempty"`
	MaxTokens           int                  `json:"max_tokens,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

func (u Usage) CachedTokens() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

type ModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

type ModelEntry struct {
	ID                               string   `json:"id"`
	Object                           string   `json:"object"`
	OwnedBy                          string   `json:"owned_by"`
	Context                          int      `json:"context,omitempty"`
	ContextWindow                    int      `json:"context_window,omitempty"`
	ContextLength                    int      `json:"context_length,omitempty"`
	MaxContextLength                 int      `json:"max_context_length,omitempty"`
	MaxContextTokens                 int      `json:"max_context_tokens,omitempty"`
	MaxModelLen                      int      `json:"max_model_len,omitempty"`
	MaxTokens                        int      `json:"max_tokens,omitempty"`
	InputTokenLimit                  int      `json:"input_token_limit,omitempty"`
	MaxInputTokens                   int      `json:"max_input_tokens,omitempty"`
	ContextTokenLimit                int      `json:"context_token_limit,omitempty"`
	ContextTokenLimitCamel           int      `json:"contextTokenLimit,omitempty"`
	ContextTokenLimitForMaxMode      int      `json:"context_token_limit_for_max_mode,omitempty"`
	ContextTokenLimitForMaxModeCamel int      `json:"contextTokenLimitForMaxMode,omitempty"`
	Efforts                          []string `json:"supported_efforts,omitempty"`
	Backend                          string   `json:"backend,omitempty"`
	ClaudeModel                      string   `json:"claude_model,omitempty"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string                        `json:"message"`
	Type    string                        `json:"type"`
	Code    string                        `json:"code,omitempty"`
	Param   string                        `json:"param,omitempty"`
	Clyde   *errcontract.ErrorDiagnostics `json:"clyde,omitempty"`
}

type ContentKind int

const (
	ContentKindEmpty ContentKind = iota
	ContentKindString
	ContentKindParts
)

func FlattenContent(raw json.RawMessage) string {
	parts, kind := NormalizeContent(raw)
	if kind == ContentKindString {
		if len(parts) == 0 {
			return ""
		}
		return parts[0].Text
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "image_url":
			b.WriteString("[image]")
		case "input_audio":
			b.WriteString("[audio]")
		case "refusal":
			b.WriteString("[refusal: ")
			b.WriteString(p.Refusal)
			b.WriteString("]")
		default:
			b.WriteString("[")
			b.WriteString(p.Type)
			b.WriteString("]")
		}
	}
	return b.String()
}

func NormalizeContent(raw json.RawMessage) ([]ContentPart, ContentKind) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ContentKindEmpty
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ContentPart{{Type: "text", Text: s}}, ContentKindString
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		for i := range parts {
			if parts[i].Type == "" {
				parts[i].Type = "text"
			}
		}
		return parts, ContentKindParts
	}
	return []ContentPart{{Type: "text", Text: string(raw)}}, ContentKindString
}
