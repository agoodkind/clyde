package contextcount

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	generic "goodkind.io/clyde/internal/contextcount"
)

const (
	anthropicAPIBaseURL = "https://api.anthropic.com"
	apiHTTPTimeout      = 60 * time.Second
	maxAPIAttempts      = 6

	blockTypeThinking         = "thinking"
	blockTypeRedactedThinking = "redacted_thinking"
	blockTypeImage            = "image"
)

var _ generic.Counter = (*APICounter)(nil)

// APICounterOptions configures an Anthropic count_tokens counter.
type APICounterOptions struct {
	Endpoint string
	Client   *http.Client
	Clock    generic.Clock
}

// APICounter counts a transcript through Anthropic count_tokens.
type APICounter struct {
	access   generic.Credentials
	endpoint string
	client   *http.Client
	clock    generic.Clock
}

// NewAPICounter creates an Anthropic count_tokens counter.
func NewAPICounter(access generic.Credentials, options APICounterOptions) *APICounter {
	endpoint := options.Endpoint
	if endpoint == "" {
		endpoint = anthropicAPIBaseURL
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: apiHTTPTimeout}
	}
	return &APICounter{
		access:   access,
		endpoint: endpoint,
		client:   client,
		clock:    options.Clock,
	}
}

// Source returns the registered source name.
func (c *APICounter) Source() generic.CounterSource {
	return generic.CounterSourceAPI
}

// Count posts the normalized transcript to Anthropic count_tokens.
func (c *APICounter) Count(ctx context.Context, transcript generic.Transcript) (int, error) {
	if transcript.Model == "" {
		err := fmt.Errorf("claude contextcount api: missing model")
		slog.ErrorContext(ctx, "providers.claude.contextcount.api.missing_model",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"err", err,
		)
		return 0, err
	}
	normalized := normalizeTranscript(transcript)
	if len(normalized.Messages) == 0 {
		slog.InfoContext(ctx, "providers.claude.contextcount.api.empty_transcript",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"model", transcript.Model,
		)
		return 0, nil
	}
	secret, err := c.readSecret(ctx)
	if err != nil {
		return 0, err
	}
	params, err := transcriptToCountTokensParams(normalized)
	if err != nil {
		slog.ErrorContext(ctx, "providers.claude.contextcount.api.request_shape_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"model", transcript.Model,
			"err", err,
		)
		return 0, fmt.Errorf("claude contextcount api: request shape: %w", err)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		slog.ErrorContext(ctx, "providers.claude.contextcount.api.marshal_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"model", transcript.Model,
			"err", err,
		)
		return 0, fmt.Errorf("claude contextcount api: marshal request: %w", err)
	}

	started := c.currentTime()
	client := anthropic.NewClient(
		option.WithAPIKey(secret),
		option.WithHTTPClient(c.client),
		option.WithMaxRetries(maxAPIAttempts),
		option.WithRequestTimeout(apiHTTPTimeout),
		option.WithBaseURL(c.endpoint),
	)
	response, err := client.Messages.CountTokens(ctx, params)
	if err != nil {
		slog.WarnContext(ctx, "providers.claude.contextcount.api.count_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"model", transcript.Model,
			"err", err,
		)
		return 0, fmt.Errorf("claude contextcount api: count tokens: %w", err)
	}
	if response == nil {
		return 0, fmt.Errorf("claude contextcount api: nil response")
	}
	if response.InputTokens <= 0 {
		slog.WarnContext(ctx, "providers.claude.contextcount.api.zero_tokens",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"model", transcript.Model,
		)
		return 0, fmt.Errorf("claude contextcount api: zero input_tokens in response")
	}
	tokens := int(response.InputTokens)
	slog.InfoContext(ctx, "providers.claude.contextcount.api.completed",
		"component", "providers.claude.contextcount",
		"subcomponent", "api",
		"model", transcript.Model,
		"input_tokens", tokens,
		"duration_ms", c.currentTime().Sub(started).Milliseconds(),
		"request_bytes", len(encoded),
	)
	return tokens, nil
}

func (c *APICounter) readSecret(ctx context.Context) (string, error) {
	if c.access == nil {
		err := fmt.Errorf("claude contextcount api: missing credentials")
		slog.ErrorContext(ctx, "providers.claude.contextcount.api.missing_access",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"err", err,
		)
		return "", err
	}
	secret, err := c.access.Secret(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "providers.claude.contextcount.api.secret_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"err", err,
		)
		return "", fmt.Errorf("claude contextcount api: credentials: %w", err)
	}
	if secret == "" {
		err := fmt.Errorf("claude contextcount api: empty credentials")
		slog.ErrorContext(ctx, "providers.claude.contextcount.api.empty_secret",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"err", err,
		)
		return "", err
	}
	return secret, nil
}

func (c *APICounter) currentTime() time.Time {
	if c.clock == nil {
		return time.Time{}
	}
	return c.clock.Now()
}

func transcriptToCountTokensParams(transcript generic.Transcript) (anthropic.MessageCountTokensParams, error) {
	messages := transcriptMessagesToParams(transcript.Messages)
	tools, err := transcriptToolsToParams(transcript.Tools)
	if err != nil {
		return anthropic.MessageCountTokensParams{}, err
	}
	return anthropic.MessageCountTokensParams{
		Model:    transcript.Model,
		System:   transcriptSystemToParams(transcript.System),
		Tools:    tools,
		Messages: messages,
	}, nil
}

func transcriptSystemToParams(blocks []generic.TextBlock) anthropic.MessageCountTokensParamsSystemUnion {
	if len(blocks) == 0 {
		return anthropic.MessageCountTokensParamsSystemUnion{}
	}
	systemBlocks := make([]anthropic.TextBlockParam, 0, len(blocks))
	for _, block := range blocks {
		systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: block.Text})
	}
	return anthropic.MessageCountTokensParamsSystemUnion{OfTextBlockArray: systemBlocks}
}

func transcriptToolsToParams(tools []generic.ToolDef) ([]anthropic.MessageCountTokensToolUnionParam, error) {
	out := make([]anthropic.MessageCountTokensToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		schema, err := unmarshalToolInputSchemaParam(tool.InputSchema)
		if err != nil {
			slog.Warn("providers.claude.contextcount.api.tool_schema_failed",
				"component", "providers.claude.contextcount",
				"subcomponent", "api",
				"tool", tool.Name,
				"err", err,
			)
			return nil, fmt.Errorf("tool %q input_schema: %w", tool.Name, err)
		}
		toolParam := anthropic.MessageCountTokensToolParamOfTool(schema, tool.Name)
		if tool.Description != "" && toolParam.OfTool != nil {
			toolParam.OfTool.Description = anthropic.String(tool.Description)
		}
		out = append(out, toolParam)
	}
	return out, nil
}

func unmarshalToolInputSchemaParam(raw json.RawMessage) (anthropic.ToolInputSchemaParam, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte(`{}`)
	}
	var schema anthropic.ToolInputSchemaParam
	if err := json.Unmarshal(trimmed, &schema); err != nil {
		slog.Warn("providers.claude.contextcount.api.tool_schema_decode_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"err", err,
		)
		return anthropic.ToolInputSchemaParam{}, fmt.Errorf("decode tool input schema: %w", err)
	}
	return schema, nil
}

func transcriptMessagesToParams(messages []generic.Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(messages))
	for _, message := range messages {
		content := transcriptContentBlocksToParams(message.Content)
		out = append(out, anthropic.MessageParam{
			Role:    anthropic.MessageParamRole(message.Role),
			Content: content,
		})
	}
	return out
}

func transcriptContentBlocksToParams(blocks []generic.ContentBlock) []anthropic.ContentBlockParamUnion {
	out := make([]anthropic.ContentBlockParamUnion, 0, len(blocks))
	for _, block := range blocks {
		converted := transcriptContentBlockToParam(block)
		out = append(out, converted)
	}
	return out
}

func transcriptContentBlockToParam(block generic.ContentBlock) anthropic.ContentBlockParamUnion {
	switch block.Type {
	case blockTypeText:
		return anthropic.NewTextBlock(block.Text)
	case blockTypeThinking:
		if block.Thinking == "" || block.Signature == "" {
			return unsupportedContentBlockParam(block.Type)
		}
		return anthropic.NewThinkingBlock(block.Signature, block.Thinking)
	case blockTypeRedactedThinking:
		if block.RedactedData == "" {
			return unsupportedContentBlockParam(block.Type)
		}
		return anthropic.NewRedactedThinkingBlock(block.RedactedData)
	case blockTypeToolUse:
		return transcriptToolUseBlockToParam(block)
	case blockTypeToolResult:
		return transcriptToolResultBlockToParam(block)
	case blockTypeImage:
		return transcriptImageBlockToParam(block)
	default:
		return unsupportedContentBlockParam(block.Type)
	}
}

func transcriptToolUseBlockToParam(block generic.ContentBlock) anthropic.ContentBlockParamUnion {
	input := bytes.TrimSpace(block.ToolInput)
	if len(input) == 0 {
		input = []byte(`{}`)
	}
	return anthropic.NewToolUseBlock(block.ToolUseID, cloneRawMessage(input), block.ToolName)
}

func transcriptToolResultBlockToParam(block generic.ContentBlock) anthropic.ContentBlockParamUnion {
	if len(block.ToolResult) == 0 {
		return anthropic.NewToolResultBlock(block.ToolUseID, block.ToolResultText, block.IsError)
	}
	content := transcriptToolResultContentBlocksToParams(block.ToolResult)
	toolResult := anthropic.ToolResultBlockParam{
		ToolUseID: block.ToolUseID,
		Content:   content,
		IsError:   anthropic.Bool(block.IsError),
	}
	return anthropic.ContentBlockParamUnion{OfToolResult: &toolResult}
}

func transcriptToolResultContentBlocksToParams(
	blocks []generic.ContentBlock,
) []anthropic.ToolResultBlockParamContentUnion {
	out := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(blocks))
	for _, block := range blocks {
		converted := transcriptToolResultContentBlockToParam(block)
		out = append(out, converted)
	}
	return out
}

func transcriptToolResultContentBlockToParam(
	block generic.ContentBlock,
) anthropic.ToolResultBlockParamContentUnion {
	switch block.Type {
	case blockTypeText:
		return anthropic.ToolResultBlockParamContentUnion{
			OfText: &anthropic.TextBlockParam{Text: block.Text},
		}
	case blockTypeImage:
		imageBlock := transcriptImageBlockToParam(block)
		if imageBlock.OfImage == nil {
			return toolResultTextContentBlock(unsupportedBlockText(block.Type))
		}
		return anthropic.ToolResultBlockParamContentUnion{OfImage: imageBlock.OfImage}
	default:
		return toolResultTextContentBlock(unsupportedBlockText(block.Type))
	}
}

func transcriptImageBlockToParam(block generic.ContentBlock) anthropic.ContentBlockParamUnion {
	if block.ImageSource.Type == "base64" && block.ImageSource.Data != "" {
		return anthropic.NewImageBlockBase64(block.ImageSource.MediaType, block.ImageSource.Data)
	}
	return unsupportedContentBlockParam(block.Type)
}

func unsupportedContentBlockParam(blockType string) anthropic.ContentBlockParamUnion {
	return anthropic.NewTextBlock(unsupportedBlockText(blockType))
}

func toolResultTextContentBlock(text string) anthropic.ToolResultBlockParamContentUnion {
	return anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: text}}
}

func unsupportedBlockText(blockType string) string {
	return fmt.Sprintf("[unsupported block type %q]", blockType)
}
