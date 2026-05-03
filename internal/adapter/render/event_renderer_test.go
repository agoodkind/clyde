package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/correlation"
)

func TestEventRendererSuppressesArgumentOnlyToolDeltaLogs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewEventRenderer("req", "alias", "codex", log)

	chunks := r.HandleEvent(Event{
		Kind: EventToolCallDelta,
		ToolCalls: []adapteropenai.ToolCall{{
			Index: 0,
			Type:  "function",
			Function: adapteropenai.ToolCallFunction{
				Arguments: strings.Repeat("x", 128),
			},
		}},
	})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	if strings.TrimSpace(buf.String()) != "" {
		t.Fatalf("argument-only delta should not log, got %s", buf.String())
	}
	r.Flush()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("summary log lines=%d want 1: %s", len(lines), buf.String())
	}
	var evt map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &evt); err != nil {
		t.Fatalf("unmarshal summary log: %v", err)
	}
	if evt["msg"] != "adapter.event.delta_summary" {
		t.Fatalf("msg=%v", evt["msg"])
	}
	if evt["event_kind"] != string(EventToolCallDelta) {
		t.Fatalf("event_kind=%v", evt["event_kind"])
	}
	if evt["delta_count"].(float64) != 1 || evt["tool_call_arg_chars"].(float64) != 128 {
		t.Fatalf("summary=%v", evt)
	}
}

func TestEventRendererLogsToolCallIdentitySummary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewEventRenderer("req", "alias", "codex", log)

	_ = r.HandleEvent(Event{
		Kind: EventToolCallDelta,
		ToolCalls: []adapteropenai.ToolCall{{
			Index: 0,
			ID:    "call_1",
			Type:  "function",
			Function: adapteropenai.ToolCallFunction{
				Name: "ApplyPatch",
			},
		}},
	})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines=%d want 2: %s", len(lines), buf.String())
	}
	var evt map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &evt); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if evt["msg"] != "adapter.event.normalized" {
		t.Fatalf("msg=%v", evt["msg"])
	}
	names, _ := evt["tool_call_names"].([]any)
	if len(names) != 1 || names[0] != "ApplyPatch" {
		t.Fatalf("tool_call_names=%v", evt["tool_call_names"])
	}
}

func TestEventRendererAssistantSummaryIncludesToolDiagnostics(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewEventRenderer("req", "alias", "codex", log)

	_ = r.HandleEvent(Event{
		Kind: EventToolCallDelta,
		ToolCalls: []adapteropenai.ToolCall{{
			Index: 0,
			ID:    "call_1",
			Type:  "function",
			Function: adapteropenai.ToolCallFunction{
				Name: "Subagent",
			},
		}},
	})
	r.LogAssistantTextSummary("tool_calls", nil)

	var summary map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal log: %v", err)
		}
		if evt["msg"] == "adapter.render.assistant_text_summary" {
			summary = evt
			break
		}
	}
	if summary == nil {
		t.Fatalf("assistant summary not found: %s", buf.String())
	}
	names, _ := summary["tool_call_names"].([]any)
	if len(names) != 1 || names[0] != "Subagent" {
		t.Fatalf("tool_call_names=%v", summary["tool_call_names"])
	}
	if summary["has_subagent_tool_call"] != true {
		t.Fatalf("has_subagent_tool_call=%v want true", summary["has_subagent_tool_call"])
	}
}

func TestEventRendererKeepsCursorThinkingMapping(t *testing.T) {
	r := NewEventRenderer("req-thinking", "alias", "codex", nil)
	chunks := r.HandleEvent(Event{Kind: EventReasoningDelta, Text: "checking constraints"})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	delta := chunks[0].Choices[0].Delta
	if !strings.Contains(delta.Content, "<!--clyde-thinking-->") {
		t.Fatalf("missing content thinking marker: %q", delta.Content)
	}
	if delta.ReasoningContent != "checking constraints" {
		t.Fatalf("reasoning_content=%q want checking constraints", delta.ReasoningContent)
	}
}

func TestEventRendererEmitsSyntheticThinkingWhenReasoningIsSignaled(t *testing.T) {
	r := NewEventRenderer("req-thinking-signal", "alias", "codex", nil)
	chunks := r.HandleEvent(Event{Kind: EventReasoningSignaled})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	got := chunks[0].Choices[0].Delta.Content
	if got != ThinkingInlineOpen() {
		t.Fatalf("thinking open=%q want %q", got, ThinkingInlineOpen())
	}
	if chunks := r.HandleEvent(Event{Kind: EventReasoningFinished}); len(chunks) != 1 {
		t.Fatalf("finish chunks=%d want close marker", len(chunks))
	} else if close := chunks[0].Choices[0].Delta.Content; !strings.Contains(close, "<!--/clyde-thinking-->") {
		t.Fatalf("missing close marker: %q", close)
	}
}

func TestEventRendererSuppressesLeadingThinkingPlaceholderBody(t *testing.T) {
	r := NewEventRenderer("req-thinking-placeholder", "alias", "codex", nil)
	chunks := r.HandleEvent(Event{Kind: EventReasoningDelta, Text: "Thinking...", ReasoningKind: "summary"})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	got := chunks[0].Choices[0].Delta.Content
	if got != ThinkingInlineOpen() {
		t.Fatalf("thinking open=%q want %q", got, ThinkingInlineOpen())
	}

	chunks = r.HandleEvent(Event{Kind: EventReasoningDelta, Text: "Evaluating task strategies", ReasoningKind: "summary"})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	got = chunks[0].Choices[0].Delta.Content
	if strings.Contains(got, "\n> Thinking...") {
		t.Fatalf("placeholder body leaked: %q", got)
	}
	if !strings.Contains(got, "Evaluating task strategies") {
		t.Fatalf("missing real reasoning: %q", got)
	}
}

func TestEventRendererLogsAssistantTextRepeatedHalfSummary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewEventRenderer("req-half", "alias", "codex", log)

	first := "first answer with enough tokens for duplicate detection"
	second := "\n\nfirst answer with enough tokens for duplicate detection"
	_ = r.HandleEvent(Event{Kind: EventAssistantTextDelta, Text: first})
	r.RecordAssistantTextDeltaEmitted(first)
	_ = r.HandleEvent(Event{Kind: EventAssistantTextDelta, Text: second})
	r.RecordAssistantTextDeltaEmitted(second)
	r.LogAssistantTextSummary("stop", &adapteropenai.Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18})

	evt := assistantTextSummaryLog(t, buf.String())
	if evt.Msg != "adapter.render.assistant_text_summary" {
		t.Fatalf("msg=%q", evt.Msg)
	}
	if evt.RequestID != "req-half" || evt.Model != "alias" || evt.Backend != "codex" {
		t.Fatalf("identity fields=%+v", evt)
	}
	if evt.DeltaCount != 2 {
		t.Fatalf("delta_count=%d want 2", evt.DeltaCount)
	}
	if !evt.RepeatedHalf {
		t.Fatalf("expected repeated_half: %+v", evt)
	}
	normalized := "first answer with enough tokens for duplicate detection first answer with enough tokens for duplicate detection"
	hash := sha256.Sum256([]byte(normalized))
	if evt.NormalizedSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("sha=%q", evt.NormalizedSHA256)
	}
	if evt.UsagePromptTokens != 10 || evt.UsageCompletionTokens != 8 || evt.UsageTotalTokens != 18 {
		t.Fatalf("usage fields=%+v", evt)
	}
}

func TestEventRendererLogsAssistantTextRepeatedSuffixSummary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewEventRenderer("req-suffix", "alias", "codex", log)

	first := "intro text before the loop. "
	second := "this suffix has enough words and enough characters "
	third := "this suffix has enough words and enough characters"
	_ = r.HandleEvent(Event{Kind: EventAssistantTextDelta, Text: first})
	r.RecordAssistantTextDeltaEmitted(first)
	_ = r.HandleEvent(Event{Kind: EventAssistantTextDelta, Text: second})
	r.RecordAssistantTextDeltaEmitted(second)
	_ = r.HandleEvent(Event{Kind: EventAssistantTextDelta, Text: third})
	r.RecordAssistantTextDeltaEmitted(third)
	r.LogAssistantTextSummary("stop", nil)

	evt := assistantTextSummaryLog(t, buf.String())
	if evt.RepeatedHalf {
		t.Fatalf("did not expect repeated_half: %+v", evt)
	}
	if !evt.RepeatedSuffix {
		t.Fatalf("expected repeated_suffix: %+v", evt)
	}
	if evt.RepeatedSuffixChars == 0 {
		t.Fatalf("repeated_suffix_chars=%d", evt.RepeatedSuffixChars)
	}
	if evt.FirstPreview == "" || evt.LastPreview == "" {
		t.Fatalf("previews missing: %+v", evt)
	}
	if strings.Contains(evt.FirstPreview, "\n") || strings.Contains(evt.LastPreview, "\n") {
		t.Fatalf("previews should be normalized: %+v", evt)
	}
}

func TestEventRendererLogsAssistantTextSummaryCorrelation(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	corr := correlation.Context{
		TraceID:              "0123456789abcdef0123456789abcdef",
		SpanID:               "0123456789abcdef",
		ParentSpanID:         "fedcba9876543210",
		RequestID:            "req-corr",
		CursorRequestID:      "cursor-req",
		CursorConversationID: "cursor-conv",
	}
	ctx := correlation.WithContext(context.Background(), corr)
	r := NewEventRendererWithContext(ctx, "req-corr", "alias", "codex", log)

	_ = r.HandleEvent(Event{Kind: EventAssistantTextDelta, Text: "correlated text"})
	r.RecordAssistantTextDeltaEmitted("correlated text")
	r.SetUpstreamResponseID("resp-upstream")
	r.LogAssistantTextSummary("stop", nil)

	evt := assistantTextSummaryLog(t, buf.String())
	if evt.TraceID != string(corr.TraceID) {
		t.Fatalf("trace_id=%q want %q", evt.TraceID, corr.TraceID)
	}
	if evt.SpanID != string(corr.SpanID) {
		t.Fatalf("span_id=%q want %q", evt.SpanID, corr.SpanID)
	}
	if evt.ParentSpanID != string(corr.ParentSpanID) {
		t.Fatalf("parent_span_id=%q want %q", evt.ParentSpanID, corr.ParentSpanID)
	}
	if evt.CursorRequestID != corr.CursorRequestID {
		t.Fatalf("cursor_request_id=%q want %q", evt.CursorRequestID, corr.CursorRequestID)
	}
	if evt.CursorConversationID != corr.CursorConversationID {
		t.Fatalf("cursor_conversation_id=%q want %q", evt.CursorConversationID, corr.CursorConversationID)
	}
	if evt.UpstreamResponseID != "resp-upstream" {
		t.Fatalf("upstream_response_id=%q want resp-upstream", evt.UpstreamResponseID)
	}
}

type assistantTextSummaryLogEntry struct {
	Msg                   string `json:"msg"`
	RequestID             string `json:"request_id"`
	TraceID               string `json:"trace_id"`
	SpanID                string `json:"span_id"`
	ParentSpanID          string `json:"parent_span_id"`
	CursorRequestID       string `json:"cursor_request_id"`
	CursorConversationID  string `json:"cursor_conversation_id"`
	UpstreamResponseID    string `json:"upstream_response_id"`
	Backend               string `json:"backend"`
	Model                 string `json:"model"`
	FinishReason          string `json:"finish_reason"`
	DeltaCount            int    `json:"assistant_text_delta_count"`
	Chars                 int    `json:"assistant_text_chars"`
	NormalizedChars       int    `json:"assistant_text_normalized_chars"`
	NormalizedSHA256      string `json:"assistant_text_normalized_sha256"`
	FirstPreview          string `json:"assistant_text_first_preview"`
	LastPreview           string `json:"assistant_text_last_preview"`
	FirstPreviewTruncated bool   `json:"assistant_text_first_preview_truncated"`
	LastPreviewTruncated  bool   `json:"assistant_text_last_preview_truncated"`
	RepeatedHalf          bool   `json:"assistant_text_repeated_half"`
	RepeatedSuffix        bool   `json:"assistant_text_repeated_suffix"`
	RepeatedSuffixChars   int    `json:"assistant_text_repeated_suffix_chars"`
	UsagePromptTokens     int    `json:"usage_prompt_tokens"`
	UsageCompletionTokens int    `json:"usage_completion_tokens"`
	UsageTotalTokens      int    `json:"usage_total_tokens"`
}

func assistantTextSummaryLog(t *testing.T, logs string) assistantTextSummaryLogEntry {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt assistantTextSummaryLogEntry
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal log: %v\n%s", err, line)
		}
		if evt.Msg == "adapter.render.assistant_text_summary" {
			return evt
		}
	}
	t.Fatalf("assistant text summary log not found: %s", logs)
	return assistantTextSummaryLogEntry{}
}
