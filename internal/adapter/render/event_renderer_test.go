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
	"goodkind.io/gklog/correlation"
)

func TestEventRendererSuppressesArgumentOnlyToolDeltaLogs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewEventRenderer("req", "alias", "codex", log)

	chunks := r.HandleEvent(ToolCallDelta{
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
	r.Flush(context.Background())
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

	_ = r.HandleEvent(ToolCallDelta{
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

	_ = r.HandleEvent(ToolCallDelta{
		ToolCalls: []adapteropenai.ToolCall{{
			Index: 0,
			ID:    "call_1",
			Type:  "function",
			Function: adapteropenai.ToolCallFunction{
				Name: "Subagent",
			},
		}},
	})
	r.LogAssistantTextSummary(context.Background(), "tool_calls", nil)

	var summary map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
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
	chunks := r.HandleEvent(ReasoningDelta{Text: "checking constraints", ReasoningKind: "text", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	delta := chunks[0].Choices[0].Delta
	if !strings.Contains(delta.Content, `<!--clyde-thinking data-origin="codex"-->`) {
		t.Fatalf("missing content thinking marker: %q", delta.Content)
	}
	want := FormatSyntheticContentDeltaWithRef(SyntheticReasoning, true, true, "", OriginCodex, "checking constraints")
	if delta.Content != want {
		t.Fatalf("content=%q want %q", delta.Content, want)
	}
	if delta.ReasoningContent != "checking constraints" {
		t.Fatalf("reasoning_content=%q want checking constraints", delta.ReasoningContent)
	}
}

func TestEventRendererDualSurfaceModePreservesSyntheticThinking(t *testing.T) {
	r := NewEventRendererWithOptions(
		"req-thinking-dual",
		"alias",
		"codex",
		nil,
		EventRendererOptions{ReasoningRenderMode: ReasoningRenderModeDualSurface},
	)
	chunks := r.HandleEvent(ReasoningDelta{Text: "checking constraints", ReasoningKind: "text", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	delta := chunks[0].Choices[0].Delta
	if !strings.Contains(delta.Content, `<!--clyde-thinking data-origin="codex"-->`) {
		t.Fatalf("missing content thinking marker: %q", delta.Content)
	}
	if delta.ReasoningContent != "checking constraints" {
		t.Fatalf("reasoning_content=%q want checking constraints", delta.ReasoningContent)
	}
	if chunks := r.HandleEvent(ReasoningSignaled{ReasoningKind: "", ItemID: "", ItemType: ""}); len(chunks) != 0 {
		t.Fatalf("signaled after open chunks=%d want 0", len(chunks))
	}
	if chunks := r.HandleEvent(ReasoningFinished{ReasoningKind: "", EncryptedContent: "", Signature: "", ItemID: "", ItemType: ""}); len(chunks) != 1 {
		t.Fatalf("finish chunks=%d want 1", len(chunks))
	} else if close := chunks[0].Choices[0].Delta.Content; close != SyntheticContentClose(SyntheticReasoning) {
		t.Fatalf("close=%q want %q", close, SyntheticContentClose(SyntheticReasoning))
	}
}

func TestEventRendererReasoningContentOnlyOmitsSyntheticThinking(t *testing.T) {
	r := NewEventRendererWithOptions(
		"req-thinking-openai",
		"alias",
		"codex",
		nil,
		EventRendererOptions{ReasoningRenderMode: ReasoningRenderModeReasoningContentOnly},
	)
	if chunks := r.HandleEvent(ReasoningSignaled{ReasoningKind: "", ItemID: "", ItemType: ""}); len(chunks) != 0 {
		t.Fatalf("signaled chunks=%d want 0", len(chunks))
	}
	chunks := r.HandleEvent(ReasoningDelta{Text: "checking constraints", ReasoningKind: "text", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	delta := chunks[0].Choices[0].Delta
	if delta.Role != "assistant" {
		t.Fatalf("role=%q want assistant", delta.Role)
	}
	if delta.Content != "" {
		t.Fatalf("content=%q want empty", delta.Content)
	}
	if delta.ReasoningContent != "checking constraints" {
		t.Fatalf("reasoning_content=%q want checking constraints", delta.ReasoningContent)
	}
	if chunks := r.HandleEvent(ReasoningFinished{ReasoningKind: "", EncryptedContent: "", Signature: "", ItemID: "", ItemType: ""}); len(chunks) != 0 {
		t.Fatalf("finish chunks=%d want 0", len(chunks))
	}
}

func TestEventRendererEmitsSyntheticThinkingWhenReasoningIsSignaled(t *testing.T) {
	r := NewEventRenderer("req-thinking-signal", "alias", "codex", nil)
	chunks := r.HandleEvent(ReasoningSignaled{ReasoningKind: "", ItemID: "", ItemType: ""})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	got := chunks[0].Choices[0].Delta.Content
	wantOpen := SyntheticContentOpenWithRef(SyntheticReasoning, "", OriginCodex)
	if got != wantOpen {
		t.Fatalf("thinking open=%q want %q", got, wantOpen)
	}
	if chunks := r.HandleEvent(ReasoningFinished{ReasoningKind: "", EncryptedContent: "", Signature: "", ItemID: "", ItemType: ""}); len(chunks) != 1 {
		t.Fatalf("finish chunks=%d want close marker", len(chunks))
	} else if close := chunks[0].Choices[0].Delta.Content; close != SyntheticContentClose(SyntheticReasoning) {
		t.Fatalf("missing close marker: %q want %q", close, SyntheticContentClose(SyntheticReasoning))
	}
}

// TestEventRendererSignaledThenDeltaKeepsBlockquotePrefix is the
// regression test for the Cursor-visible thinking block losing its
// blockquote bar at the first body line. When handleReasoningSignaled
// ships the open marker in its own chunk and a subsequent reasoning
// delta brings the body, the concatenated content must keep every body
// line `> `-prefixed up to the close marker so the markdown renderer
// keeps the blockquote intact.
func TestEventRendererSignaledThenDeltaKeepsBlockquotePrefix(t *testing.T) {
	r := NewEventRenderer("req-thinking-split", "alias", "codex", nil)
	chunks := r.HandleEvent(ReasoningSignaled{ReasoningKind: "", ItemID: "", ItemType: ""})
	if len(chunks) != 1 {
		t.Fatalf("signal chunks=%d want 1", len(chunks))
	}
	openContent := chunks[0].Choices[0].Delta.Content

	bodyChunks := r.HandleEvent(ReasoningDelta{Text: "The user is right.\nSecond body line.", ReasoningKind: "text", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	if len(bodyChunks) != 1 {
		t.Fatalf("body chunks=%d want 1", len(bodyChunks))
	}
	bodyContent := bodyChunks[0].Choices[0].Delta.Content

	closeChunks := r.HandleEvent(ReasoningFinished{ReasoningKind: "", EncryptedContent: "", Signature: "", ItemID: "", ItemType: ""})
	if len(closeChunks) != 1 {
		t.Fatalf("close chunks=%d want 1", len(closeChunks))
	}
	closeContent := closeChunks[0].Choices[0].Delta.Content

	full := openContent + bodyContent + closeContent
	openIdx := strings.Index(full, "-->")
	if openIdx < 0 {
		t.Fatalf("open marker not found: %q", full)
	}
	closeIdx := strings.Index(full, "<!--/")
	if closeIdx < 0 {
		t.Fatalf("close marker not found: %q", full)
	}
	envelope := full[openIdx+len("-->") : closeIdx]
	for idx, line := range strings.Split(envelope, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, ">") {
			t.Fatalf("envelope line %d not blockquote-prefixed: %q\nfull: %q", idx, line, full)
		}
	}
}

func TestEventRendererClosesSyntheticThinkingBeforeToolCalls(t *testing.T) {
	r := NewEventRenderer("req-thinking-tool", "alias", "codex", nil)
	_ = r.HandleEvent(ReasoningSignaled{ReasoningKind: "", ItemID: "", ItemType: ""})

	chunks := r.HandleEvent(ToolCallDelta{
		ToolCalls: []adapteropenai.ToolCall{{
			Index: 0,
			ID:    "call_1",
			Type:  "function",
			Function: adapteropenai.ToolCallFunction{
				Name:      "Read",
				Arguments: `{"path":"README.md"}`,
			},
		}},
	})
	if len(chunks) != 2 {
		t.Fatalf("chunks=%d want close marker plus tool call", len(chunks))
	}
	if close := chunks[0].Choices[0].Delta.Content; close != SyntheticContentClose(SyntheticReasoning) {
		t.Fatalf("first chunk should close thinking: %q", close)
	}
	if len(chunks[1].Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("second chunk should carry tool call: %+v", chunks[1])
	}
}

func TestEventRendererSuppressesLeadingThinkingPlaceholderBody(t *testing.T) {
	r := NewEventRenderer("req-thinking-placeholder", "alias", "codex", nil)
	chunks := r.HandleEvent(ReasoningDelta{Text: "Thinking...", ReasoningKind: "summary", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	got := chunks[0].Choices[0].Delta.Content
	wantOpen := SyntheticContentOpenWithRef(SyntheticReasoning, "", OriginCodex)
	if got != wantOpen {
		t.Fatalf("thinking open=%q want %q", got, wantOpen)
	}

	chunks = r.HandleEvent(ReasoningDelta{Text: "Evaluating task strategies", ReasoningKind: "summary", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
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

func TestEventRendererDoesNotPrefixFirstBoldSummaryAfterReasoningSignal(t *testing.T) {
	r := NewEventRenderer("req-thinking-heading", "alias", "codex", nil)
	_ = r.HandleEvent(ReasoningSignaled{ReasoningKind: "", ItemID: "", ItemType: ""})
	chunks := r.HandleEvent(ReasoningDelta{Text: "**Checking git changes**", ReasoningKind: "summary", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d want 1", len(chunks))
	}
	got := chunks[0].Choices[0].Delta.Content
	if strings.HasPrefix(got, "\n\n") {
		t.Fatalf("first bold summary has leading break: %q", got)
	}
	if !strings.Contains(got, "**Checking git changes**") {
		t.Fatalf("missing bold summary: %q", got)
	}
}

func TestEventRendererReasoningContentOnlyDoesNotCloseBeforeText(t *testing.T) {
	r := NewEventRendererWithOptions(
		"req-thinking-openai-text",
		"alias",
		"codex",
		nil,
		EventRendererOptions{ReasoningRenderMode: ReasoningRenderModeReasoningContentOnly},
	)
	reasoningChunks := r.HandleEvent(ReasoningDelta{Text: "internal reasoning", ReasoningKind: "text", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	if len(reasoningChunks) != 1 {
		t.Fatalf("reasoning chunks=%d want 1", len(reasoningChunks))
	}
	textChunks := r.HandleEvent(TextDelta{Text: "final answer"})
	if len(textChunks) != 1 {
		t.Fatalf("text chunks=%d want 1", len(textChunks))
	}
	delta := textChunks[0].Choices[0].Delta
	if delta.Content != "final answer" {
		t.Fatalf("content=%q want final answer", delta.Content)
	}
	if strings.Contains(delta.Content, "<!--/clyde-thinking") {
		t.Fatalf("unexpected synthetic close in text chunk: %q", delta.Content)
	}
}

func TestEventRendererReasoningContentOnlyDoesNotCloseBeforeToolCall(t *testing.T) {
	r := NewEventRendererWithOptions(
		"req-thinking-openai-tool",
		"alias",
		"codex",
		nil,
		EventRendererOptions{ReasoningRenderMode: ReasoningRenderModeReasoningContentOnly},
	)
	reasoningChunks := r.HandleEvent(ReasoningDelta{Text: "internal reasoning", ReasoningKind: "text", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	if len(reasoningChunks) != 1 {
		t.Fatalf("reasoning chunks=%d want 1", len(reasoningChunks))
	}
	toolChunks := r.HandleEvent(ToolCallDelta{
		ToolCalls: []adapteropenai.ToolCall{{
			Index: 0,
			ID:    "call_1",
			Type:  "function",
			Function: adapteropenai.ToolCallFunction{
				Name:      "Read",
				Arguments: `{"path":"README.md"}`,
			},
		}},
	})
	if len(toolChunks) != 1 {
		t.Fatalf("tool chunks=%d want 1", len(toolChunks))
	}
	delta := toolChunks[0].Choices[0].Delta
	if len(delta.ToolCalls) != 1 {
		t.Fatalf("tool_calls=%d want 1", len(delta.ToolCalls))
	}
	if delta.Content != "" {
		t.Fatalf("content=%q want empty", delta.Content)
	}
}

func TestEventRendererLogsAssistantTextRepeatedHalfSummary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewEventRenderer("req-half", "alias", "codex", log)

	first := "first answer with enough tokens for duplicate detection"
	second := "\n\nfirst answer with enough tokens for duplicate detection"
	_ = r.HandleEvent(TextDelta{Text: first})
	r.RecordAssistantTextDeltaEmitted(first)
	_ = r.HandleEvent(TextDelta{Text: second})
	r.RecordAssistantTextDeltaEmitted(second)
	r.LogAssistantTextSummary(context.Background(), "stop", &adapteropenai.Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18})

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
	_ = r.HandleEvent(TextDelta{Text: first})
	r.RecordAssistantTextDeltaEmitted(first)
	_ = r.HandleEvent(TextDelta{Text: second})
	r.RecordAssistantTextDeltaEmitted(second)
	_ = r.HandleEvent(TextDelta{Text: third})
	r.RecordAssistantTextDeltaEmitted(third)
	r.LogAssistantTextSummary(context.Background(), "stop", nil)

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
		TraceID:      "0123456789abcdef0123456789abcdef",
		SpanID:       "0123456789abcdef",
		ParentSpanID: "fedcba9876543210",
		RequestID:    "req-corr",
		IdentityAttributes: []correlation.IdentityAttribute{
			{Key: "cursor_request_id", Value: "cursor-req"},
			{Key: "cursor_conversation_id", Value: "cursor-conv"},
		},
	}
	ctx := correlation.WithContext(context.Background(), corr)
	r := NewEventRendererWithContext(ctx, "req-corr", "alias", "codex", log)

	_ = r.HandleEvent(TextDelta{Text: "correlated text"})
	r.RecordAssistantTextDeltaEmitted("correlated text")
	r.SetUpstreamResponseID(ctx, "resp-upstream")
	r.LogAssistantTextSummary(ctx, "stop", nil)

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
	if evt.CursorRequestID != "cursor-req" {
		t.Fatalf("cursor_request_id=%q want cursor-req", evt.CursorRequestID)
	}
	if evt.CursorConversationID != "cursor-conv" {
		t.Fatalf("cursor_conversation_id=%q want cursor-conv", evt.CursorConversationID)
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

// TestEventRendererCapturesAnthropicSignatureOnReasoningClose asserts the
// Anthropic-side capture path: a stream of ReasoningDelta events
// carrying a Signature followed by ReasoningFinished must produce a
// close chunk whose `data-signature` attribute is the most recent
// signature value. The synthetic-thinking close-marker is the cross-turn
// carrier the inbound mapper relies on.
func TestEventRendererCapturesAnthropicSignatureOnReasoningClose(t *testing.T) {
	r := NewEventRenderer("req-anth-sig", "alias", "anthropic", nil)
	// First reasoning delta opens the synthetic envelope and ships the
	// thinking body. Anthropic pairs each thinking_delta with a separate
	// signature_delta in the same content_block so the signature flows
	// in via subsequent ReasoningDelta events with empty Text.
	_ = r.HandleEvent(ReasoningDelta{Text: "interim deliberation", ReasoningKind: "text", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	_ = r.HandleEvent(ReasoningDelta{Text: "", ReasoningKind: "text", SummaryIndex: nil, Signature: "first-sig", RedactedData: "", ItemID: "", ItemType: ""})
	_ = r.HandleEvent(ReasoningDelta{Text: "", ReasoningKind: "text", SummaryIndex: nil, Signature: "final-sig", RedactedData: "", ItemID: "", ItemType: ""})
	chunks := r.HandleEvent(ReasoningFinished{ReasoningKind: "", EncryptedContent: "", Signature: "", ItemID: "", ItemType: ""})
	if len(chunks) != 1 {
		t.Fatalf("close chunks=%d want 1: %+v", len(chunks), chunks)
	}
	closeContent := chunks[0].Choices[0].Delta.Content
	if !strings.Contains(closeContent, `data-signature="final-sig"`) {
		t.Fatalf("close marker missing latest signature: %q", closeContent)
	}
	if strings.Contains(closeContent, "first-sig") {
		t.Fatalf("close marker should carry only the most recent signature, found stale value: %q", closeContent)
	}
	if strings.Contains(closeContent, "data-encrypted=") {
		t.Fatalf("Anthropic-only span must not emit data-encrypted: %q", closeContent)
	}
}
