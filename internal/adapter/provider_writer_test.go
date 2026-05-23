package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/correlation"
)

func TestNormalizedProviderFinishReasonPreservesToolCallTerminalState(t *testing.T) {
	t.Parallel()

	got := normalizedProviderFinishReason(adapterprovider.Result{
		FinishReason:  "stop",
		ToolCallCount: 1,
	})
	if got != "tool_calls" {
		t.Fatalf("finish_reason=%q want tool_calls", got)
	}
}

func TestCodexProviderAdapterErrorMapsContextWindowError(t *testing.T) {
	t.Parallel()

	aerr := codexProviderAdapterError(&adaptercodex.ContextWindowError{
		Message: "Your input exceeds the context window of this model. Please adjust your input and try again.",
	})

	if aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", aerr.HTTPStatus, http.StatusBadRequest)
	}
	if aerr.OpenAIType != "invalid_request_error" || aerr.OpenAICode != "context_length_exceeded" || aerr.OpenAIParam != "messages" {
		t.Fatalf("adapter error=%+v", aerr)
	}
	if aerr.Message != "This model's maximum context length was exceeded. Please reduce the length of the messages." {
		t.Fatalf("message=%q", aerr.Message)
	}
}

func TestCodexProviderAdapterErrorMapsWrappedContextWindowError(t *testing.T) {
	t.Parallel()

	wrapped := errors.Join(errors.New("transport failed"), &adaptercodex.ContextWindowError{
		Message: "context_length_exceeded",
	})
	aerr := codexProviderAdapterError(wrapped)

	if aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", aerr.HTTPStatus, http.StatusBadRequest)
	}
	if aerr.OpenAICode != "context_length_exceeded" {
		t.Fatalf("code=%q want context_length_exceeded", aerr.OpenAICode)
	}
}

func TestCodexProviderAdapterErrorMapsWrappedUnsupportedModelError(t *testing.T) {
	t.Parallel()

	wrapped := errors.Join(errors.New("codex websocket warmup failed"), &adaptercodex.UnsupportedModelError{
		Message: "The '5.5' model is not supported when using Codex with a ChatGPT account.",
	})
	aerr := codexProviderAdapterError(wrapped)

	if aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", aerr.HTTPStatus, http.StatusBadRequest)
	}
	if aerr.OpenAIType != "invalid_request_error" || aerr.OpenAICode != "model_not_supported" || aerr.OpenAIParam != "model" {
		t.Fatalf("adapter error=%+v", aerr)
	}
	if aerr.Message != "The '5.5' model is not supported when using Codex with a ChatGPT account." {
		t.Fatalf("message=%q", aerr.Message)
	}
}

func TestCodexProviderAdapterErrorMapsGenericProviderError(t *testing.T) {
	t.Parallel()

	aerr := codexProviderAdapterError(errors.New("codex websocket read failed"))

	// Cursor BYOK never sees HTTP 5xx + server_error; the boundary
	// flips generic codex provider failures into a Cursor-safe
	// invalid_request_error envelope so the upstream message lands
	// in the chat transcript rather than triggering Cursor's BYOK
	// fallback chrome.
	if aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", aerr.HTTPStatus, http.StatusBadRequest)
	}
	if aerr.OpenAIType != "invalid_request_error" || aerr.OpenAICode != "upstream_failed" || aerr.OpenAIParam != "" {
		t.Fatalf("adapter error=%+v", aerr)
	}
	if !strings.Contains(aerr.Message, "codex websocket read failed") {
		t.Fatalf("message=%q", aerr.Message)
	}
}

func TestAdapterErrUpstreamFailedUsesOpenAICompatibleServerError(t *testing.T) {
	t.Parallel()

	aerr := adapterErrUpstreamFailed("codex", "codex websocket read failed", errors.New("boom"))
	aerr.applyDefaults()

	if aerr.OpenAIType != "server_error" || aerr.OpenAICode != "upstream_failed" || aerr.OpenAIParam != "" {
		t.Fatalf("adapter error=%+v", aerr)
	}
	if aerr.Message != "codex websocket read failed" {
		t.Fatalf("message=%q", aerr.Message)
	}
}

func TestProviderStreamWriterWritesMappedErrorEnvelope(t *testing.T) {
	t.Parallel()

	srv, _ := newLoggingServer(t, config.LoggingConfig{})
	rec := httptest.NewRecorder()
	sse, err := adapteropenai.NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	writer := &providerStreamWriter{sse: sse, server: srv, log: srv.log}

	aerr := newAdapterError(adapterErrorModelNotSupported, "unsupported model: gpt-5.5")
	err = writer.writeStreamError(context.Background(), aerr)
	if err != nil {
		t.Fatalf("writeStreamError: %v", err)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`"error":{`,
		`"type":"invalid_request_error"`,
		`"code":"model_not_supported"`,
		`"param":"model"`,
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body missing %q: %s", want, body)
		}
	}
}

func TestProviderStreamWriterLogsSSEChunkFlushShapeWithoutContent(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := httptest.NewRecorder()
	sse, err := adapteropenai.NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	writer := &providerStreamWriter{
		sse:        sse,
		reqID:      "req-flush-log",
		modelAlias: "alias-flush-log",
		log:        log,
		ctx:        context.Background(),
	}
	finishReason := "stop"
	err = writer.writeRenderedChunk(context.Background(), adapteropenai.StreamChunk{
		ID:      "req-flush-log",
		Object:  "chat.completion.chunk",
		Created: 1,
		Model:   "alias-flush-log",
		Choices: []adapteropenai.StreamChoice{{
			Index: 0,
			Delta: adapteropenai.StreamDelta{
				Content:          "secret-content",
				ReasoningContent: "secret-reasoning",
				ToolCalls: []adapteropenai.ToolCall{{
					Function: adapteropenai.ToolCallFunction{Arguments: "secret-args"},
				}},
			},
			FinishReason: &finishReason,
		}},
		Usage: &adapteropenai.Usage{TotalTokens: 3},
	})
	if err != nil {
		t.Fatalf("writeRenderedChunk: %v", err)
	}
	logs := buf.String()
	for _, leaked := range []string{"secret-content", "secret-reasoning", "secret-args"} {
		if strings.Contains(logs, leaked) {
			t.Fatalf("flush log leaked %q: %s", leaked, logs)
		}
	}
	got := streamChunkFlushLogs(t, logs)[0]
	if got.RequestID != "req-flush-log" || got.Model != "alias-flush-log" {
		t.Fatalf("identity fields=%+v", got)
	}
	if got.SSEPayloadKind != "chat.completion.chunk" || got.StreamDone {
		t.Fatalf("payload fields=%+v", got)
	}
	if got.SSEChunkSequence != 1 || got.ChoiceCount != 1 || got.ToolCallCount != 1 {
		t.Fatalf("count fields=%+v", got)
	}
	if got.DeltaContentLength != len("secret-content") || got.DeltaReasoningLength != len("secret-reasoning") {
		t.Fatalf("delta lengths=%+v", got)
	}
	if !got.DeltaContentPresent || !got.DeltaReasoningContentPresent || got.DeltaReasoningPresent {
		t.Fatalf("delta presence fields=%+v", got)
	}
	if !got.DeltaToolCallsPresent || got.DeltaRolePresent || got.DeltaRefusalPresent {
		t.Fatalf("tool/refusal/role presence fields=%+v", got)
	}
	if got.SSEFlushedAt == "" {
		t.Fatalf("missing sse_flushed_at: %+v", got)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.SSEFlushedAt); err != nil {
		t.Fatalf("sse_flushed_at=%q: %v", got.SSEFlushedAt, err)
	}
	if got.FinishReason != "stop" || !got.UsagePresent {
		t.Fatalf("finish/usage fields=%+v", got)
	}
}

func TestProviderStreamWriterLogsToolCallChunkIDsAndNames(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := httptest.NewRecorder()
	sse, err := adapteropenai.NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	writer := &providerStreamWriter{
		sse:        sse,
		reqID:      "req-tool-log",
		modelAlias: "alias-tool-log",
		log:        log,
		ctx:        context.Background(),
	}

	err = writer.writeRenderedChunk(context.Background(), adapteropenai.StreamChunk{
		ID:      "req-tool-log",
		Object:  "chat.completion.chunk",
		Created: 1,
		Model:   "alias-tool-log",
		Choices: []adapteropenai.StreamChoice{{
			Index: 0,
			Delta: adapteropenai.StreamDelta{
				ToolCalls: []adapteropenai.ToolCall{{
					Index: 0,
					ID:    "call_lookup",
					Type:  "function",
					Function: adapteropenai.ToolCallFunction{
						Name:      "lookupThing",
						Arguments: `{"secret":"redacted from logs"}`,
					},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("writeRenderedChunk: %v", err)
	}

	logs := buf.String()
	if strings.Contains(logs, "redacted from logs") {
		t.Fatalf("flush log leaked tool arguments: %s", logs)
	}
	got := streamChunkFlushLogs(t, logs)[0]
	if got.ToolCallCount != 1 || !got.DeltaToolCallsPresent {
		t.Fatalf("tool call shape=%+v", got)
	}
	if len(got.ToolCallIDs) != 1 || got.ToolCallIDs[0] != "call_lookup" {
		t.Fatalf("tool_call_ids=%v", got.ToolCallIDs)
	}
	if len(got.ToolCallNames) != 1 || got.ToolCallNames[0] != "lookupThing" {
		t.Fatalf("tool_call_names=%v", got.ToolCallNames)
	}
	if got.DeltaContentPresent || got.DeltaReasoningPresent || got.DeltaReasoningContentPresent {
		t.Fatalf("unexpected text presence=%+v", got)
	}
}

func TestProviderStreamWriterLogsFinishChunkAndDoneFlush(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := httptest.NewRecorder()
	sse, err := adapteropenai.NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	writer := &providerStreamWriter{
		sse:        sse,
		reqID:      "req-finish-log",
		modelAlias: "alias-finish-log",
		log:        log,
		ctx:        context.Background(),
	}

	err = writer.finalizeStream(context.Background(), adapterprovider.Result{FinishReason: "stop"}, false)
	if err != nil {
		t.Fatalf("finalizeStream: %v", err)
	}

	entries := streamChunkFlushLogs(t, buf.String())
	if len(entries) != 2 {
		t.Fatalf("log count=%d want 2: %+v", len(entries), entries)
	}
	finish := entries[0]
	if finish.SSEChunkSequence != 1 || finish.StreamDone || finish.SSEPayloadKind != "chat.completion.chunk" {
		t.Fatalf("finish payload fields=%+v", finish)
	}
	if finish.FinishReason != "stop" || finish.ChoiceCount != 1 || finish.UsagePresent {
		t.Fatalf("finish shape=%+v", finish)
	}
	if finish.DeltaContentPresent || finish.DeltaToolCallsPresent || finish.DeltaRefusalPresent || finish.DeltaReasoningPresent || finish.DeltaReasoningContentPresent {
		t.Fatalf("finish delta presence=%+v", finish)
	}
	done := entries[1]
	if done.SSEChunkSequence != 2 || !done.StreamDone || done.SSEPayloadKind != "done" {
		t.Fatalf("done payload fields=%+v", done)
	}
	if done.RequestID != "req-finish-log" || done.Model != "alias-finish-log" {
		t.Fatalf("done identity=%+v", done)
	}
	if done.ChoiceCount != 0 || done.ToolCallCount != 0 || done.FinishReason != "" || done.UsagePresent {
		t.Fatalf("done shape=%+v", done)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream did not finish: %s", rec.Body.String())
	}
}

type streamChunkFlushLogEntry struct {
	Message                      string   `json:"msg"`
	RequestID                    string   `json:"request_id"`
	Model                        string   `json:"model"`
	SSEChunkSequence             int      `json:"sse_chunk_sequence"`
	SSEPayloadKind               string   `json:"sse_payload_kind"`
	StreamDone                   bool     `json:"stream_done"`
	SSEFlushedAt                 string   `json:"sse_flushed_at"`
	ChoiceCount                  int      `json:"choice_count"`
	DeltaRolePresent             bool     `json:"delta_role_present"`
	DeltaContentPresent          bool     `json:"delta_content_present"`
	DeltaToolCallsPresent        bool     `json:"delta_tool_calls_present"`
	DeltaRefusalPresent          bool     `json:"delta_refusal_present"`
	DeltaReasoningContentPresent bool     `json:"delta_reasoning_content_present"`
	DeltaReasoningPresent        bool     `json:"delta_reasoning_present"`
	DeltaContentLength           int      `json:"delta_content_length"`
	DeltaReasoningLength         int      `json:"delta_reasoning_length"`
	ToolCallCount                int      `json:"tool_call_count"`
	ToolCallIDs                  []string `json:"tool_call_ids"`
	ToolCallNames                []string `json:"tool_call_names"`
	FinishReason                 string   `json:"finish_reason"`
	UsagePresent                 bool     `json:"usage_present"`
}

func streamChunkFlushLogs(t *testing.T, logs string) []streamChunkFlushLogEntry {
	t.Helper()
	var entries []streamChunkFlushLogEntry
	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt streamChunkFlushLogEntry
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal log: %v\n%s", err, line)
		}
		if evt.Message == "adapter.openai.sse.stream_chunk_flushed" {
			entries = append(entries, evt)
		}
	}
	if len(entries) > 0 {
		return entries
	}
	t.Fatalf("stream chunk flush log not found: %s", logs)
	return nil
}

func TestProviderStreamWriterLogsAssistantTextSummaryAtFinalize(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := httptest.NewRecorder()
	sse, err := adapteropenai.NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	corr := correlation.Context{
		TraceID:            "11111111111111111111111111111111",
		SpanID:             "2222222222222222",
		ParentSpanID:       "3333333333333333",
		RequestID:          "req-final",
		UpstreamRequestID:  "",
		UpstreamResponseID: "",
		ChatKey:            "",
		ChatKeySource:      "",
		ChatRootKey:        "",
		ChatBranchKey:      "",
		IdentityAttributes: []correlation.IdentityAttribute{
			{Key: "cursor_request_id", Value: "cursor-final"},
			{Key: "cursor_conversation_id", Value: "conversation-final"},
		},
	}
	ctx := correlation.WithContext(context.Background(), corr)
	writer := &providerStreamWriter{
		sse:        sse,
		renderer:   adapterrender.NewEventRendererWithContext(ctx, "req-final", "alias-final", "codex", log),
		reqID:      "req-final",
		modelAlias: "alias-final",
		ctx:        ctx,
	}

	if err := writer.WriteEvent(adapterrender.TextDelta{Text: "finalized text"}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.ToolCallDelta{
		ToolCalls: []adapteropenai.ToolCall{{
			ID:   "call_task",
			Type: "function",
			Function: adapteropenai.ToolCallFunction{
				Name:      "Task",
				Arguments: `{"prompt":"redacted from summary logs"}`,
			},
		}},
	}); err != nil {
		t.Fatalf("WriteEvent tool call: %v", err)
	}
	err = writer.finalizeStream(ctx, adapterprovider.Result{
		FinishReason: "stop",
		Usage: adapteropenai.Usage{
			PromptTokens:     3,
			CompletionTokens: 2,
			TotalTokens:      5,
		},
		UpstreamResponseID: "resp-final",
	}, true)
	if err != nil {
		t.Fatalf("finalizeStream: %v", err)
	}

	evt := providerAssistantTextSummaryLog(t, buf.String())
	if evt.RequestID != "req-final" || evt.Model != "alias-final" || evt.Backend != "codex" {
		t.Fatalf("identity fields=%+v", evt)
	}
	if evt.FinishReason != "stop" {
		t.Fatalf("finish_reason=%q", evt.FinishReason)
	}
	if evt.DeltaCount != 1 || evt.Chars != len("finalized text") {
		t.Fatalf("assistant text counts=%+v", evt)
	}
	if evt.UsagePromptTokens != 3 || evt.UsageCompletionTokens != 2 || evt.UsageTotalTokens != 5 {
		t.Fatalf("usage fields=%+v", evt)
	}
	if evt.TraceID != string(corr.TraceID) || evt.SpanID != string(corr.SpanID) || evt.ParentSpanID != string(corr.ParentSpanID) {
		t.Fatalf("trace fields=%+v", evt)
	}
	if evt.CursorRequestID != "cursor-final" || evt.CursorConversationID != "conversation-final" {
		t.Fatalf("cursor fields=%+v", evt)
	}
	if evt.UpstreamResponseID != "resp-final" {
		t.Fatalf("upstream_response_id=%q want resp-final", evt.UpstreamResponseID)
	}
	if evt.ToolCallNameCount != 1 || len(evt.ToolCallNames) != 1 || evt.ToolCallNames[0] != "Task" {
		t.Fatalf("tool call summary=%+v", evt)
	}
	if !evt.HasSubagentToolCall {
		t.Fatalf("has_subagent_tool_call=false want true")
	}
	if strings.Contains(buf.String(), "redacted from summary logs") {
		t.Fatalf("assistant summary leaked tool arguments: %s", buf.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream did not finish: %s", rec.Body.String())
	}
}

func TestProviderStreamWriterFinalizedNoticeDoesNotRepeatAssistantRole(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := httptest.NewRecorder()
	sse, err := adapteropenai.NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	writer := &providerStreamWriter{
		sse:        sse,
		renderer:   adapterrender.NewEventRenderer("req-notice-role", "alias-notice-role", "codex", log),
		reqID:      "req-notice-role",
		modelAlias: "alias-notice-role",
		log:        log,
		ctx:        context.Background(),
	}

	if err := writer.WriteEvent(adapterrender.TextDelta{Text: "answer"}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	err = writer.finalizeStream(context.Background(), adapterprovider.Result{
		FinishReason: "stop",
		UsageNotices: []adapterruntime.UsageNotice{{
			Provider:   "codex",
			WindowKey:  "weekly",
			LimitLabel: "weekly",
			Text:       "warning text",
		}},
	}, false)
	if err != nil {
		t.Fatalf("finalizeStream: %v", err)
	}

	entries := streamChunkFlushLogs(t, buf.String())
	if len(entries) != 4 {
		t.Fatalf("log count=%d want 4: %+v", len(entries), entries)
	}
	if !entries[0].DeltaRolePresent || !entries[0].DeltaContentPresent {
		t.Fatalf("answer chunk shape=%+v", entries[0])
	}
	notice := entries[1]
	if notice.DeltaRolePresent || !notice.DeltaContentPresent {
		t.Fatalf("notice chunk repeated role or omitted content: %+v", notice)
	}
	if strings.Count(rec.Body.String(), `"role":"assistant"`) != 1 {
		t.Fatalf("stream should contain one assistant role: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "clyde-notice") {
		t.Fatalf("stream missing notice marker: %s", rec.Body.String())
	}
}

type providerAssistantTextSummaryLogEntry struct {
	Msg                   string   `json:"msg"`
	RequestID             string   `json:"request_id"`
	TraceID               string   `json:"trace_id"`
	SpanID                string   `json:"span_id"`
	ParentSpanID          string   `json:"parent_span_id"`
	CursorRequestID       string   `json:"cursor_request_id"`
	CursorConversationID  string   `json:"cursor_conversation_id"`
	UpstreamResponseID    string   `json:"upstream_response_id"`
	Backend               string   `json:"backend"`
	Model                 string   `json:"model"`
	FinishReason          string   `json:"finish_reason"`
	DeltaCount            int      `json:"assistant_text_delta_count"`
	Chars                 int      `json:"assistant_text_chars"`
	UsagePromptTokens     int      `json:"usage_prompt_tokens"`
	UsageCompletionTokens int      `json:"usage_completion_tokens"`
	UsageTotalTokens      int      `json:"usage_total_tokens"`
	ToolCallNameCount     int      `json:"tool_call_name_count"`
	ToolCallNames         []string `json:"tool_call_names"`
	HasSubagentToolCall   bool     `json:"has_subagent_tool_call"`
}

func providerAssistantTextSummaryLog(t *testing.T, logs string) providerAssistantTextSummaryLogEntry {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt providerAssistantTextSummaryLogEntry
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal log: %v\n%s", err, line)
		}
		if evt.Msg == "adapter.render.assistant_text_summary" {
			return evt
		}
	}
	t.Fatalf("assistant text summary log not found: %s", logs)
	return providerAssistantTextSummaryLogEntry{}
}
