package contextcount

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	generic "goodkind.io/clyde/internal/contextcount"
)

const expectedAnthropicVersionHeader = "2023-06-01"

type staticCredentials struct {
	value string
}

func (s staticCredentials) Secret(context.Context) (string, error) {
	return s.value, nil
}

type fixedClock struct {
	now time.Time
}

func (f fixedClock) Now() time.Time {
	return f.now
}

type apiTestServerState struct {
	t             *testing.T
	statuses      []int
	attempts      int
	lastRequest   sdkCountRequest
	lastAPIKey    string
	lastVersion   string
	lastPath      string
	responseToken int
}

type sdkCountRequest struct {
	Model    string            `json:"model"`
	System   []sdkTextBlock    `json:"system"`
	Tools    []sdkToolDef      `json:"tools"`
	Messages []sdkCountMessage `json:"messages"`
}

type sdkTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type sdkToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type sdkCountMessage struct {
	Role    string            `json:"role"`
	Content []sdkContentBlock `json:"content"`
}

type sdkContentBlock struct {
	Type      string            `json:"type"`
	Text      string            `json:"text"`
	Thinking  string            `json:"thinking"`
	Signature string            `json:"signature"`
	Data      string            `json:"data"`
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Input     json.RawMessage   `json:"input"`
	ToolUseID string            `json:"tool_use_id"`
	Content   []sdkContentBlock `json:"content"`
	IsError   bool              `json:"is_error"`
	Source    sdkImageSource    `json:"source"`
}

type sdkImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

func TestAPICounterUsesSDKClientOptions(t *testing.T) {
	t.Parallel()

	state := &apiTestServerState{
		t:             t,
		statuses:      []int{http.StatusOK},
		responseToken: 42,
	}
	server := httptest.NewServer(http.HandlerFunc(state.handle))
	defer server.Close()

	counter := NewAPICounter(staticCredentials{value: "fixture-value"}, APICounterOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
		Clock:    fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	tokens, err := counter.Count(context.Background(), simpleTranscript())
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if tokens != 42 {
		t.Fatalf("Count() = %d, want 42", tokens)
	}
	if state.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.attempts)
	}
	if state.lastPath != "/v1/messages/count_tokens" {
		t.Fatalf("path = %q, want /v1/messages/count_tokens", state.lastPath)
	}
	if state.lastAPIKey != "fixture-value" {
		t.Fatalf("x-api-key = %q, want fixture-value", state.lastAPIKey)
	}
	if state.lastVersion != expectedAnthropicVersionHeader {
		t.Fatalf("anthropic-version = %q, want %q", state.lastVersion, expectedAnthropicVersionHeader)
	}
	if state.lastRequest.Model != "claude-haiku-4-5" {
		t.Fatalf("model = %q, want claude-haiku-4-5", state.lastRequest.Model)
	}
	if len(state.lastRequest.System) != 1 || state.lastRequest.System[0].Text != "system" {
		t.Fatalf("system = %#v, want one system text block", state.lastRequest.System)
	}
	if len(state.lastRequest.Tools) != 1 || state.lastRequest.Tools[0].Name != "Read" {
		t.Fatalf("tools = %#v, want one Read tool", state.lastRequest.Tools)
	}
	if len(state.lastRequest.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(state.lastRequest.Messages))
	}
	content := state.lastRequest.Messages[0].Content
	if len(content) != 1 || content[0].Type != blockTypeText || content[0].Text != "hello" {
		t.Fatalf("message content = %#v, want one text block", content)
	}
}

func TestAPICounterFatalOn4xx(t *testing.T) {
	t.Parallel()

	state := &apiTestServerState{
		t:        t,
		statuses: []int{http.StatusBadRequest},
	}
	server := httptest.NewServer(http.HandlerFunc(state.handle))
	defer server.Close()
	counter := NewAPICounter(staticCredentials{value: "fixture-value"}, APICounterOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
		Clock:    fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	_, err := counter.Count(context.Background(), simpleTranscript())
	if err == nil {
		t.Fatalf("Count() error = nil, want fatal 4xx")
	}
	if state.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.attempts)
	}
}

func TestAPICounterEmptyNormalizedTranscriptReturnsZero(t *testing.T) {
	t.Parallel()

	state := &apiTestServerState{
		t:             t,
		statuses:      []int{http.StatusOK},
		responseToken: 99,
	}
	server := httptest.NewServer(http.HandlerFunc(state.handle))
	defer server.Close()
	counter := NewAPICounter(staticCredentials{value: "fixture-value"}, APICounterOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
		Clock:    fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	tokens, err := counter.Count(context.Background(), generic.Transcript{
		Model:    "claude-haiku-4-5",
		System:   nil,
		Tools:    nil,
		Messages: nil,
	})
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if tokens != 0 {
		t.Fatalf("Count() = %d, want 0", tokens)
	}
	if state.attempts != 0 {
		t.Fatalf("attempts = %d, want 0", state.attempts)
	}
}

func TestTranscriptToCountTokensParamsMapsSDKBlocks(t *testing.T) {
	t.Parallel()

	params, err := transcriptToCountTokensParams(generic.Transcript{
		Model:  "claude-haiku-4-5",
		System: []generic.TextBlock{{Text: "system"}},
		Tools: []generic.ToolDef{
			{Name: "Read", Description: "read files", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []generic.Message{
			{Role: messageRoleAssistant, Content: []generic.ContentBlock{
				textBlock("assistant text"),
				thinkingBlock("thinking text", "signature-value"),
				redactedThinkingBlock("redacted-value"),
				emptyInputToolUseBlock("tool-1"),
			}},
			{Role: messageRoleUser, Content: []generic.ContentBlock{
				toolResultBlock("tool-1", "result text"),
			}},
			{Role: messageRoleUser, Content: []generic.ContentBlock{
				nestedToolResultBlock("tool-2", []generic.ContentBlock{
					textBlock("nested text"),
					imageBlock("base64", "image/png", "aW1hZ2U=", ""),
				}),
			}},
		},
	})
	if err != nil {
		t.Fatalf("transcriptToCountTokensParams() error = %v", err)
	}
	request := marshalSDKCountRequest(t, params)
	if request.Model != "claude-haiku-4-5" {
		t.Fatalf("model = %q, want claude-haiku-4-5", request.Model)
	}
	if len(request.System) != 1 || request.System[0].Text != "system" {
		t.Fatalf("system = %#v, want one system text block", request.System)
	}
	if len(request.Tools) != 1 || request.Tools[0].Description != "read files" {
		t.Fatalf("tools = %#v, want one described tool", request.Tools)
	}
	assistantContent := request.Messages[0].Content
	assertBlock(t, assistantContent[0], blockTypeText, "assistant text")
	if assistantContent[1].Type != blockTypeThinking || assistantContent[1].Thinking != "thinking text" || assistantContent[1].Signature != "signature-value" {
		t.Fatalf("thinking block = %#v, want thinking text and signature", assistantContent[1])
	}
	if assistantContent[2].Type != blockTypeRedactedThinking || assistantContent[2].Data != "redacted-value" {
		t.Fatalf("redacted thinking block = %#v, want redacted data", assistantContent[2])
	}
	if assistantContent[3].Type != blockTypeToolUse || string(assistantContent[3].Input) != `{}` {
		t.Fatalf("tool_use block = %#v, want empty input object", assistantContent[3])
	}
	stringResult := request.Messages[1].Content[0]
	if stringResult.Type != blockTypeToolResult || len(stringResult.Content) != 1 {
		t.Fatalf("string tool_result = %#v, want one nested content block", stringResult)
	}
	assertBlock(t, stringResult.Content[0], blockTypeText, "result text")
	nestedResult := request.Messages[2].Content[0]
	if nestedResult.Type != blockTypeToolResult || len(nestedResult.Content) != 2 {
		t.Fatalf("nested tool_result = %#v, want two nested content blocks", nestedResult)
	}
	assertBlock(t, nestedResult.Content[0], blockTypeText, "nested text")
	image := nestedResult.Content[1]
	if image.Type != blockTypeImage || image.Source.Type != "base64" || image.Source.MediaType != "image/png" || image.Source.Data != "aW1hZ2U=" {
		t.Fatalf("nested image block = %#v, want base64 image", image)
	}
}

func TestTranscriptToCountTokensParamsUsesPlaceholders(t *testing.T) {
	t.Parallel()

	params, err := transcriptToCountTokensParams(generic.Transcript{
		Model: "claude-haiku-4-5",
		Messages: []generic.Message{
			{Role: messageRoleAssistant, Content: []generic.ContentBlock{
				thinkingBlock("thinking text", ""),
				redactedThinkingBlock(""),
				imageBlock("url", "", "", "https://example.invalid/image.png"),
				{Type: "server_tool_use"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transcriptToCountTokensParams() error = %v", err)
	}
	request := marshalSDKCountRequest(t, params)
	content := request.Messages[0].Content
	if len(content) != 4 {
		t.Fatalf("content = %d, want 4", len(content))
	}
	assertBlock(t, content[0], blockTypeText, `[unsupported block type "thinking"]`)
	assertBlock(t, content[1], blockTypeText, `[unsupported block type "redacted_thinking"]`)
	assertBlock(t, content[2], blockTypeText, `[unsupported block type "image"]`)
	assertBlock(t, content[3], blockTypeText, `[unsupported block type "server_tool_use"]`)
}

func TestNormalizeMotdShapeHoistsSidechainResult(t *testing.T) {
	t.Parallel()

	transcript := generic.Transcript{
		Model:  "claude-haiku-4-5",
		System: nil,
		Tools:  nil,
		Messages: []generic.Message{
			{Role: messageRoleAssistant, Content: []generic.ContentBlock{toolUseBlock("tool-1")}},
			{Role: messageRoleUser, Content: []generic.ContentBlock{textBlock("sidechain reminder")}},
			{Role: messageRoleUser, Content: []generic.ContentBlock{toolResultBlock("tool-1", "result")}},
		},
	}
	normalized := normalizeTranscript(transcript)
	if len(normalized.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(normalized.Messages))
	}
	content := normalized.Messages[1].Content
	if len(content) != 2 {
		t.Fatalf("user content = %d, want 2", len(content))
	}
	if content[0].Type != blockTypeToolResult {
		t.Fatalf("first block type = %q, want tool_result", content[0].Type)
	}
	if content[1].Type != blockTypeText {
		t.Fatalf("second block type = %q, want text", content[1].Type)
	}
}

func TestNormalizeRepairsOrphanToolUse(t *testing.T) {
	t.Parallel()

	transcript := generic.Transcript{
		Model:  "claude-haiku-4-5",
		System: nil,
		Tools:  nil,
		Messages: []generic.Message{
			{Role: messageRoleAssistant, Content: []generic.ContentBlock{toolUseBlock("missing-tool")}},
		},
	}
	normalized := normalizeTranscript(transcript)
	if len(normalized.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(normalized.Messages))
	}
	block := normalized.Messages[1].Content[0]
	if block.Type != blockTypeToolResult {
		t.Fatalf("repair block type = %q, want tool_result", block.Type)
	}
	if block.ToolUseID != "missing-tool" {
		t.Fatalf("repair tool id = %q, want missing-tool", block.ToolUseID)
	}
	if block.ToolResultText != syntheticToolResultPlaceholderText {
		t.Fatalf("repair text = %q, want placeholder", block.ToolResultText)
	}
	if !block.IsError {
		t.Fatalf("repair IsError = false, want true")
	}
}

func TestNormalizeStripsMessageZeroOrphanToolResult(t *testing.T) {
	t.Parallel()

	transcript := generic.Transcript{
		Model:  "claude-haiku-4-5",
		System: nil,
		Tools:  nil,
		Messages: []generic.Message{
			{Role: messageRoleUser, Content: []generic.ContentBlock{toolResultBlock("orphan", "orphan result")}},
			{Role: messageRoleAssistant, Content: []generic.ContentBlock{textBlock("assistant text")}},
		},
	}
	normalized := normalizeTranscript(transcript)
	if len(normalized.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(normalized.Messages))
	}
	firstContent := normalized.Messages[0].Content
	if len(firstContent) != 1 {
		t.Fatalf("message 0 content = %d, want 1", len(firstContent))
	}
	if firstContent[0].Type != blockTypeText {
		t.Fatalf("message 0 block type = %q, want text", firstContent[0].Type)
	}
	if firstContent[0].Text != orphanedToolResultRemovedText {
		t.Fatalf("message 0 text = %q, want orphan placeholder", firstContent[0].Text)
	}
}

func (s *apiTestServerState) handle(w http.ResponseWriter, r *http.Request) {
	s.attempts++
	s.lastPath = r.URL.Path
	s.lastAPIKey = r.Header.Get("x-api-key")
	s.lastVersion = r.Header.Get("anthropic-version")
	if err := json.NewDecoder(r.Body).Decode(&s.lastRequest); err != nil {
		s.t.Fatalf("decode request: %v", err)
	}
	status := s.statuses[len(s.statuses)-1]
	if s.attempts <= len(s.statuses) {
		status = s.statuses[s.attempts-1]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = w.Write([]byte(`{"input_tokens":` + jsonInt(s.responseToken) + `}`))
		return
	}
	_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
}

func marshalSDKCountRequest(t *testing.T, params json.Marshaler) sdkCountRequest {
	t.Helper()
	data, err := params.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}
	var request sdkCountRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatalf("decode SDK request: %v", err)
	}
	return request
}

func assertBlock(t *testing.T, block sdkContentBlock, blockType string, text string) {
	t.Helper()
	if block.Type != blockType || block.Text != text {
		t.Fatalf("block = %#v, want type %q text %q", block, blockType, text)
	}
}

func jsonInt(value int) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func simpleTranscript() generic.Transcript {
	return generic.Transcript{
		Model:  "claude-haiku-4-5",
		System: []generic.TextBlock{{Text: "system"}},
		Tools: []generic.ToolDef{
			{Name: "Read", Description: "read files", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []generic.Message{
			{Role: messageRoleUser, Content: []generic.ContentBlock{textBlock("hello")}},
		},
	}
}

func textBlock(text string) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           blockTypeText,
		Text:           text,
		Thinking:       "",
		Signature:      "",
		RedactedData:   "",
		ToolUseID:      "",
		ToolName:       "",
		ToolInput:      nil,
		ToolResult:     nil,
		ToolResultText: "",
		IsError:        false,
		ImageSource:    emptyImageSource(),
	}
}

func thinkingBlock(thinking string, signature string) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           blockTypeThinking,
		Text:           "",
		Thinking:       thinking,
		Signature:      signature,
		RedactedData:   "",
		ToolUseID:      "",
		ToolName:       "",
		ToolInput:      nil,
		ToolResult:     nil,
		ToolResultText: "",
		IsError:        false,
		ImageSource:    emptyImageSource(),
	}
}

func redactedThinkingBlock(data string) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           blockTypeRedactedThinking,
		Text:           "",
		Thinking:       "",
		Signature:      "",
		RedactedData:   data,
		ToolUseID:      "",
		ToolName:       "",
		ToolInput:      nil,
		ToolResult:     nil,
		ToolResultText: "",
		IsError:        false,
		ImageSource:    emptyImageSource(),
	}
}

func toolUseBlock(id string) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           blockTypeToolUse,
		Text:           "",
		Thinking:       "",
		Signature:      "",
		RedactedData:   "",
		ToolUseID:      id,
		ToolName:       "Read",
		ToolInput:      json.RawMessage(`{"file_path":"README.md"}`),
		ToolResult:     nil,
		ToolResultText: "",
		IsError:        false,
		ImageSource:    emptyImageSource(),
	}
}

func emptyInputToolUseBlock(id string) generic.ContentBlock {
	block := toolUseBlock(id)
	block.ToolInput = nil
	return block
}

func toolResultBlock(id string, text string) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           blockTypeToolResult,
		Text:           "",
		Thinking:       "",
		Signature:      "",
		RedactedData:   "",
		ToolUseID:      id,
		ToolName:       "",
		ToolInput:      nil,
		ToolResult:     nil,
		ToolResultText: text,
		IsError:        false,
		ImageSource:    emptyImageSource(),
	}
}

func nestedToolResultBlock(id string, content []generic.ContentBlock) generic.ContentBlock {
	block := toolResultBlock(id, "")
	block.ToolResult = content
	return block
}

func imageBlock(sourceType string, mediaType string, data string, url string) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           blockTypeImage,
		Text:           "",
		Thinking:       "",
		Signature:      "",
		RedactedData:   "",
		ToolUseID:      "",
		ToolName:       "",
		ToolInput:      nil,
		ToolResult:     nil,
		ToolResultText: "",
		IsError:        false,
		ImageSource: generic.ImageSource{
			Type:      sourceType,
			MediaType: mediaType,
			Data:      data,
			URL:       url,
		},
	}
}
