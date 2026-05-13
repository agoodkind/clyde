package contextcount

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	generic "goodkind.io/clyde/internal/contextcount"
)

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
	lastRequest   apiCountRequest
	lastAPIKey    string
	lastVersion   string
	lastPath      string
	responseToken int
}

func TestAPICounterHappyPath(t *testing.T) {
	t.Parallel()

	state := &apiTestServerState{
		t:             t,
		statuses:      []int{http.StatusOK},
		attempts:      0,
		lastRequest:   apiCountRequest{},
		lastAPIKey:    "",
		lastVersion:   "",
		lastPath:      "",
		responseToken: 42,
	}
	server := httptest.NewServer(http.HandlerFunc(state.handle))
	defer server.Close()

	counter := NewAPICounter(staticCredentials{value: "fixture-value"}, APICounterOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
		Sleep:    nil,
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
	if state.lastPath != "/" {
		t.Fatalf("path = %q, want /", state.lastPath)
	}
	if state.lastAPIKey != "fixture-value" {
		t.Fatalf("x-api-key = %q, want fixture-value", state.lastAPIKey)
	}
	if state.lastVersion != anthropicVersionHeaderValue {
		t.Fatalf("anthropic-version = %q, want %q", state.lastVersion, anthropicVersionHeaderValue)
	}
	if state.lastRequest.Model != "claude-haiku-4-5" {
		t.Fatalf("model = %q, want claude-haiku-4-5", state.lastRequest.Model)
	}
}

func TestAPICounterRetriesOn429(t *testing.T) {
	t.Parallel()

	state := &apiTestServerState{
		t:             t,
		statuses:      []int{http.StatusTooManyRequests, http.StatusOK},
		attempts:      0,
		lastRequest:   apiCountRequest{},
		lastAPIKey:    "",
		lastVersion:   "",
		lastPath:      "",
		responseToken: 77,
	}
	server := httptest.NewServer(http.HandlerFunc(state.handle))
	defer server.Close()
	sleeps := make([]time.Duration, 0, 1)
	counter := NewAPICounter(staticCredentials{value: "fixture-value"}, APICounterOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
		Sleep: func(_ context.Context, wait time.Duration) error {
			sleeps = append(sleeps, wait)
			return nil
		},
		Clock: fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	tokens, err := counter.Count(context.Background(), simpleTranscript())
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if tokens != 77 {
		t.Fatalf("Count() = %d, want 77", tokens)
	}
	if state.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", state.attempts)
	}
	if len(sleeps) != 1 {
		t.Fatalf("sleeps = %d, want 1", len(sleeps))
	}
}

func TestAPICounterRetriesOn5xx(t *testing.T) {
	t.Parallel()

	state := &apiTestServerState{
		t:             t,
		statuses:      []int{http.StatusBadGateway, http.StatusOK},
		attempts:      0,
		lastRequest:   apiCountRequest{},
		lastAPIKey:    "",
		lastVersion:   "",
		lastPath:      "",
		responseToken: 88,
	}
	server := httptest.NewServer(http.HandlerFunc(state.handle))
	defer server.Close()
	counter := NewAPICounter(staticCredentials{value: "fixture-value"}, APICounterOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
		Clock: fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	tokens, err := counter.Count(context.Background(), simpleTranscript())
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if tokens != 88 {
		t.Fatalf("Count() = %d, want 88", tokens)
	}
	if state.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", state.attempts)
	}
}

func TestAPICounterFatalOn4xx(t *testing.T) {
	t.Parallel()

	state := &apiTestServerState{
		t:             t,
		statuses:      []int{http.StatusBadRequest},
		attempts:      0,
		lastRequest:   apiCountRequest{},
		lastAPIKey:    "",
		lastVersion:   "",
		lastPath:      "",
		responseToken: 0,
	}
	server := httptest.NewServer(http.HandlerFunc(state.handle))
	defer server.Close()
	counter := NewAPICounter(staticCredentials{value: "fixture-value"}, APICounterOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
		Sleep: func(context.Context, time.Duration) error {
			return errors.New("unexpected sleep")
		},
		Clock: fixedClock{now: time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)},
	})
	_, err := counter.Count(context.Background(), simpleTranscript())
	if err == nil {
		t.Fatalf("Count() error = nil, want fatal 4xx")
	}
	if state.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.attempts)
	}
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
	w.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = w.Write([]byte(`{"input_tokens":` + jsonInt(s.responseToken) + `}`))
		return
	}
	_, _ = w.Write([]byte(`{"error":"bad"}`))
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
