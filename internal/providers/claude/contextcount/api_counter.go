package contextcount

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	generic "goodkind.io/clyde/internal/contextcount"
)

const (
	anthropicMessagesCountEndpoint = "https://api.anthropic.com/v1/messages/count_tokens"
	anthropicVersionHeaderValue    = "2023-06-01"
	apiHTTPTimeout                 = 60 * time.Second
	maxAPIAttempts                 = 6
)

var _ generic.Counter = (*APICounter)(nil)

type retrySleepFunc func(context.Context, time.Duration) error

// APICounterOptions configures an Anthropic count_tokens counter.
type APICounterOptions struct {
	Endpoint string
	Client   *http.Client
	Sleep    retrySleepFunc
	Clock    generic.Clock
}

// APICounter counts a transcript through Anthropic count_tokens.
type APICounter struct {
	access   generic.Credentials
	endpoint string
	client   *http.Client
	sleep    retrySleepFunc
	clock    generic.Clock
}

// NewAPICounter creates an Anthropic count_tokens counter.
func NewAPICounter(access generic.Credentials, options APICounterOptions) *APICounter {
	endpoint := options.Endpoint
	if endpoint == "" {
		endpoint = anthropicMessagesCountEndpoint
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: apiHTTPTimeout}
	}
	return &APICounter{
		access:   access,
		endpoint: endpoint,
		client:   client,
		sleep:    options.Sleep,
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
	secret, err := c.readSecret(ctx)
	if err != nil {
		return 0, err
	}
	normalized := normalizeTranscript(transcript)
	body, err := apiRequestFromTranscript(normalized)
	if err != nil {
		slog.ErrorContext(ctx, "providers.claude.contextcount.api.request_shape_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"model", transcript.Model,
			"err", err,
		)
		return 0, fmt.Errorf("claude contextcount api: request shape: %w", err)
	}
	encoded, err := json.Marshal(body)
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
	var tokens int
	var attemptsUsed int
	for attempt := 1; attempt <= maxAPIAttempts; attempt++ {
		countedTokens, retryAfter, err := c.countOnce(ctx, encoded, secret, attempt)
		if err != nil {
			return 0, err
		}
		if retryAfter > 0 {
			if err := c.sleepForRetry(ctx, retryAfter); err != nil {
				return 0, err
			}
			continue
		}
		tokens = countedTokens
		attemptsUsed = attempt
		break
	}
	if tokens == 0 {
		return 0, fmt.Errorf("claude contextcount api: exhausted attempts")
	}
	slog.InfoContext(ctx, "providers.claude.contextcount.api.completed",
		"component", "providers.claude.contextcount",
		"subcomponent", "api",
		"model", transcript.Model,
		"input_tokens", tokens,
		"attempt", attemptsUsed,
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

func (c *APICounter) countOnce(
	ctx context.Context,
	encoded []byte,
	secret string,
	attempt int,
) (int, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(encoded))
	if err != nil {
		slog.ErrorContext(ctx, "providers.claude.contextcount.api.request_build_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"attempt", attempt,
			"err", err,
		)
		return 0, 0, fmt.Errorf("claude contextcount api: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", secret)
	request.Header.Set("Anthropic-Version", anthropicVersionHeaderValue)

	response, err := c.client.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "providers.claude.contextcount.api.http_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"attempt", attempt,
			"err", err,
		)
		return 0, 0, fmt.Errorf("claude contextcount api: HTTP: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		slog.WarnContext(ctx, "providers.claude.contextcount.api.read_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"attempt", attempt,
			"err", err,
		)
		return 0, 0, fmt.Errorf("claude contextcount api: read response: %w", err)
	}
	return c.handleResponse(ctx, response, responseBody, attempt)
}

func (c *APICounter) handleResponse(
	ctx context.Context,
	response *http.Response,
	responseBody []byte,
	attempt int,
) (int, time.Duration, error) {
	if shouldRetryStatus(response.StatusCode) {
		if attempt >= maxAPIAttempts {
			slog.WarnContext(ctx, "providers.claude.contextcount.api.retry_exhausted",
				"component", "providers.claude.contextcount",
				"subcomponent", "api",
				"attempt", attempt,
				"status", response.StatusCode,
			)
			return 0, 0, fmt.Errorf("claude contextcount api: status %d after %d attempts", response.StatusCode, attempt)
		}
		return 0, c.retryDelay(response, attempt), nil
	}
	if response.StatusCode >= http.StatusBadRequest {
		slog.WarnContext(ctx, "providers.claude.contextcount.api.bad_status",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"attempt", attempt,
			"status", response.StatusCode,
			"body_excerpt", truncateResponseBody(responseBody),
		)
		return 0, 0, fmt.Errorf("claude contextcount api: status %d: %s", response.StatusCode, truncateResponseBody(responseBody))
	}
	var parsed apiCountResponse
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		slog.WarnContext(ctx, "providers.claude.contextcount.api.decode_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"attempt", attempt,
			"err", err,
		)
		return 0, 0, fmt.Errorf("claude contextcount api: decode response: %w", err)
	}
	if parsed.InputTokens <= 0 {
		slog.WarnContext(ctx, "providers.claude.contextcount.api.zero_tokens",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"attempt", attempt,
		)
		return 0, 0, fmt.Errorf("claude contextcount api: zero input_tokens in response")
	}
	return parsed.InputTokens, 0, nil
}

func shouldRetryStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func (c *APICounter) retryDelay(response *http.Response, attempt int) time.Duration {
	header := response.Header.Get("Retry-After")
	if wait := parseRetryAfter(header); wait > 0 {
		return wait
	}
	return backoffFor(attempt)
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	var seconds int
	if _, err := fmt.Sscanf(header, "%d", &seconds); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	parsedTime, err := http.ParseTime(header)
	if err != nil {
		return 0
	}
	duration := time.Until(parsedTime)
	if duration <= 0 {
		return 0
	}
	return duration
}

func backoffFor(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 500 * time.Millisecond
	case 2:
		return time.Second
	case 3:
		return 2 * time.Second
	case 4:
		return 4 * time.Second
	case 5:
		return 8 * time.Second
	default:
		return 15 * time.Second
	}
}

func (c *APICounter) sleepForRetry(ctx context.Context, wait time.Duration) error {
	if c.sleep != nil {
		if err := c.sleep(ctx, wait); err != nil {
			slog.WarnContext(ctx, "providers.claude.contextcount.api.sleep_failed",
				"component", "providers.claude.contextcount",
				"subcomponent", "api",
				"wait_ms", wait.Milliseconds(),
				"err", err,
			)
			return fmt.Errorf("claude contextcount api: retry sleep: %w", err)
		}
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		err := ctx.Err()
		slog.WarnContext(ctx, "providers.claude.contextcount.api.sleep_cancelled",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"wait_ms", wait.Milliseconds(),
			"err", err,
		)
		return fmt.Errorf("claude contextcount api: retry cancelled: %w", err)
	case <-timer.C:
		return nil
	}
}

func (c *APICounter) currentTime() time.Time {
	if c.clock == nil {
		return time.Time{}
	}
	return c.clock.Now()
}

func truncateResponseBody(body []byte) string {
	const maxBytes = 400
	if len(body) <= maxBytes {
		return string(body)
	}
	return string(body[:maxBytes]) + "..."
}

type apiCountResponse struct {
	InputTokens int `json:"input_tokens"`
}

type apiCountRequest struct {
	Model    string            `json:"model"`
	System   []apiTextBlock    `json:"system,omitempty"`
	Tools    []apiToolDef      `json:"tools,omitempty"`
	Messages []apiCountMessage `json:"messages"`
}

type apiTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type apiToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type apiCountMessage struct {
	Role    string            `json:"role"`
	Content []apiContentBlock `json:"content"`
}

type apiContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *apiImageSource `json:"source,omitempty"`
}

type apiImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type apiContentBlockType string

const (
	apiContentBlockText             apiContentBlockType = "text"
	apiContentBlockThinking         apiContentBlockType = "thinking"
	apiContentBlockRedactedThinking apiContentBlockType = "redacted_thinking"
	apiContentBlockToolUse          apiContentBlockType = "tool_use"
	apiContentBlockToolResult       apiContentBlockType = "tool_result"
	apiContentBlockImage            apiContentBlockType = "image"
)

func apiRequestFromTranscript(transcript generic.Transcript) (apiCountRequest, error) {
	systemBlocks := make([]apiTextBlock, 0, len(transcript.System))
	for _, block := range transcript.System {
		systemBlocks = append(systemBlocks, apiTextBlock{Type: apiContentBlockTextString(), Text: block.Text})
	}
	tools := make([]apiToolDef, 0, len(transcript.Tools))
	for _, tool := range transcript.Tools {
		tools = append(tools, apiToolDef{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: cloneRawMessage(tool.InputSchema),
		})
	}
	messages := make([]apiCountMessage, 0, len(transcript.Messages))
	for _, message := range transcript.Messages {
		content, err := apiContentBlocksFromTranscript(message.Content)
		if err != nil {
			return apiCountRequest{}, err
		}
		messages = append(messages, apiCountMessage{
			Role:    message.Role,
			Content: content,
		})
	}
	return apiCountRequest{
		Model:    transcript.Model,
		System:   systemBlocks,
		Tools:    tools,
		Messages: messages,
	}, nil
}

func apiContentBlockTextString() string {
	return string(apiContentBlockText)
}

func apiContentBlocksFromTranscript(blocks []generic.ContentBlock) ([]apiContentBlock, error) {
	apiBlocks := make([]apiContentBlock, 0, len(blocks))
	for _, block := range blocks {
		apiBlock, err := apiContentBlockFromTranscript(block)
		if err != nil {
			return nil, err
		}
		apiBlocks = append(apiBlocks, apiBlock)
	}
	return apiBlocks, nil
}

func apiContentBlockFromTranscript(block generic.ContentBlock) (apiContentBlock, error) {
	switch apiContentBlockType(block.Type) {
	case apiContentBlockText:
		apiBlock := emptyAPIContentBlock(apiContentBlockText)
		apiBlock.Text = block.Text
		return apiBlock, nil
	case apiContentBlockThinking:
		apiBlock := emptyAPIContentBlock(apiContentBlockThinking)
		apiBlock.Thinking = block.Thinking
		apiBlock.Signature = block.Signature
		return apiBlock, nil
	case apiContentBlockRedactedThinking:
		apiBlock := emptyAPIContentBlock(apiContentBlockRedactedThinking)
		apiBlock.Data = block.RedactedData
		return apiBlock, nil
	case apiContentBlockToolUse:
		return apiToolUseBlock(block), nil
	case apiContentBlockToolResult:
		return apiToolResultBlock(block)
	case apiContentBlockImage:
		return apiImageBlock(block), nil
	default:
		return apiContentBlock{}, fmt.Errorf("unsupported content block type %q", block.Type)
	}
}

func apiToolUseBlock(block generic.ContentBlock) apiContentBlock {
	apiBlock := emptyAPIContentBlock(apiContentBlockToolUse)
	input := block.ToolInput
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage(`{}`)
	}
	apiBlock.ID = block.ToolUseID
	apiBlock.Name = block.ToolName
	apiBlock.Input = cloneRawMessage(input)
	return apiBlock
}

func apiToolResultBlock(block generic.ContentBlock) (apiContentBlock, error) {
	content, err := marshalToolResultContent(block)
	if err != nil {
		slog.Warn("providers.claude.contextcount.api.tool_result_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "api",
			"err", err,
		)
		return apiContentBlock{}, fmt.Errorf("marshal tool_result content: %w", err)
	}
	apiBlock := emptyAPIContentBlock(apiContentBlockToolResult)
	apiBlock.ToolUseID = block.ToolUseID
	apiBlock.Content = content
	apiBlock.IsError = block.IsError
	return apiBlock, nil
}

func marshalToolResultContent(block generic.ContentBlock) (json.RawMessage, error) {
	if len(block.ToolResult) > 0 {
		content, err := apiContentBlocksFromTranscript(block.ToolResult)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(content)
		if err != nil {
			slog.Warn("providers.claude.contextcount.api.tool_result_blocks_encode_failed",
				"component", "providers.claude.contextcount",
				"subcomponent", "api",
				"err", err,
			)
			return nil, fmt.Errorf("encode tool_result blocks: %w", err)
		}
		return encoded, nil
	}
	if block.ToolResultText != "" {
		encoded, err := json.Marshal(block.ToolResultText)
		if err != nil {
			slog.Warn("providers.claude.contextcount.api.tool_result_text_encode_failed",
				"component", "providers.claude.contextcount",
				"subcomponent", "api",
				"err", err,
			)
			return nil, fmt.Errorf("encode tool_result text: %w", err)
		}
		return encoded, nil
	}
	return nil, nil
}

func apiImageBlock(block generic.ContentBlock) apiContentBlock {
	apiBlock := emptyAPIContentBlock(apiContentBlockImage)
	apiBlock.Source = &apiImageSource{
		Type:      block.ImageSource.Type,
		MediaType: block.ImageSource.MediaType,
		Data:      block.ImageSource.Data,
		URL:       block.ImageSource.URL,
	}
	return apiBlock
}

func emptyAPIContentBlock(blockType apiContentBlockType) apiContentBlock {
	return apiContentBlock{
		Type:      string(blockType),
		Text:      "",
		Thinking:  "",
		Signature: "",
		Data:      "",
		ID:        "",
		Name:      "",
		Input:     nil,
		ToolUseID: "",
		Content:   nil,
		IsError:   false,
		Source:    nil,
	}
}
