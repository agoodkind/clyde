package render

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/correlation"
)

// This file contains only renderer diagnostics: structured event logs,
// high-volume delta summaries, correlation plumbing, and final assistant-text
// diagnostics. It must not decide what content is rendered to the client.

type deltaSummary struct {
	Count        int
	Chars        int
	MaxChars     int
	ToolCalls    int
	ToolArgChars int
}

func (r *EventRenderer) SetUpstreamResponseID(ctx context.Context, responseID string) {
	if r == nil {
		return
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return
	}
	r.upstreamResponseID = responseID
	corr := correlation.FromContext(ctx).WithUpstreamResponseID(responseID)
	r.ctx = correlation.WithContext(ctx, corr)
}

func (r *EventRenderer) Flush(ctx context.Context) {
	r.flushSuppressedEventSummaries(ctx)
}

func (r *EventRenderer) LogAssistantTextSummary(ctx context.Context, finishReason string, usage *adapteropenai.Usage) {
	if r == nil || r.assistantTextLogged {
		return
	}
	r.assistantTextLogged = true
	summary := r.assistantText.summary()
	attrs := []slog.Attr{
		slog.String("component", "adapter"),
		slog.String("subcomponent", "renderer"),
		slog.String("request_id", r.reqID),
		slog.String("backend", r.backend),
		slog.String("model", r.modelAlias),
		slog.String("alias", r.modelAlias),
		slog.String("finish_reason", strings.TrimSpace(finishReason)),
		slog.Int("assistant_text_delta_count", summary.DeltaCount),
		slog.Int("assistant_text_chars", summary.Chars),
		slog.Int("assistant_text_normalized_chars", summary.NormalizedChars),
		slog.String("assistant_text_normalized_sha256", summary.NormalizedSHA256),
		slog.String("assistant_text_first_preview", summary.FirstPreview),
		slog.String("assistant_text_last_preview", summary.LastPreview),
		slog.Bool("assistant_text_first_preview_truncated", summary.FirstPreviewTruncated),
		slog.Bool("assistant_text_last_preview_truncated", summary.LastPreviewTruncated),
		slog.Bool("assistant_text_repeated_half", summary.RepeatedHalf),
		slog.Bool("assistant_text_repeated_suffix", summary.RepeatedSuffix),
		slog.Int("assistant_text_repeated_suffix_chars", summary.RepeatedSuffixChars),
		slog.Int("tool_call_name_count", len(r.toolCallNames)),
		slog.Any("tool_call_names", r.toolCallNames),
		slog.Bool("has_subagent_tool_call", r.hasSubagentToolCall),
	}
	corr := correlation.FromContext(ctx)
	if r.upstreamResponseID != "" {
		corr = corr.WithUpstreamResponseID(r.upstreamResponseID)
	}
	attrs = correlation.AppendAttrs(attrs, corr)
	if usage != nil {
		attrs = append(attrs,
			slog.Int("usage_prompt_tokens", usage.PromptTokens),
			slog.Int("usage_completion_tokens", usage.CompletionTokens),
			slog.Int("usage_total_tokens", usage.TotalTokens),
			slog.Int("usage_input_tokens", usage.InputTokens),
			slog.Int("usage_output_tokens", usage.OutputTokens),
			slog.Int("usage_cache_read_tokens", usage.CacheReadTokens),
			slog.Int("usage_cache_write_tokens", usage.CacheWriteTokens),
		)
	}
	r.log.LogAttrs(ctx, slog.LevelInfo, "adapter.render.assistant_text_summary", attrs...)
}

func (r *EventRenderer) recordToolCallNames(toolCalls []adapteropenai.ToolCall) {
	if r == nil {
		return
	}
	for _, tc := range toolCalls {
		name := strings.TrimSpace(tc.Function.Name)
		if name == "" {
			continue
		}
		if !slices.Contains(r.toolCallNames, name) {
			r.toolCallNames = append(r.toolCallNames, name)
		}
		if isSubagentToolCallName(name) {
			r.hasSubagentToolCall = true
		}
	}
}

func isSubagentToolCallName(name string) bool {
	switch strings.TrimSpace(name) {
	case "Task", "Subagent", "spawn_agent":
		return true
	default:
		return false
	}
}

func (r *EventRenderer) logNormalized(ev Event) {
	kind := ev.eventKind()
	itemType, itemID, deltaLen := eventDiagnosticFields(ev)
	var toolCalls []adapteropenai.ToolCall
	if td, ok := ev.(ToolCallDelta); ok {
		toolCalls = td.ToolCalls
	}
	attrs := []slog.Attr{
		slog.String("component", "adapter"),
		slog.String("subcomponent", "renderer"),
		slog.String("request_id", r.reqID),
		slog.String("backend", r.backend),
		slog.String("model", r.modelAlias),
		slog.String("alias", r.modelAlias),
		slog.String("event_kind", string(kind)),
		slog.String("item_type", itemType),
		slog.String("item_id", itemID),
		slog.Bool("reasoning_signaled", r.reasoningSignaled || kind == EventReasoningSignaled || kind == EventReasoningDelta),
		slog.Bool("reasoning_visible", r.reasoningVisible || kind == EventReasoningDelta),
		slog.Int("delta_len", deltaLen),
	}
	attrs = append(attrs, toolCallLogAttrs(toolCalls)...)
	r.log.LogAttrs(r.logContext(), slog.LevelDebug, "adapter.event.normalized", attrs...)
}

// eventDiagnosticFields extracts the itemType, itemID, and delta length for
// the given event for use in diagnostic log attributes.
func eventDiagnosticFields(ev Event) (itemType, itemID string, deltaLen int) {
	switch e := ev.(type) {
	case TextDelta:
		return "", "", len(e.Text)
	case RefusalDelta:
		return "", "", len(e.Text)
	case ReasoningSignaled:
		return e.ItemType, e.ItemID, 0
	case ReasoningDelta:
		return e.ItemType, e.ItemID, len(e.Text)
	case ReasoningFinished:
		return e.ItemType, e.ItemID, 0
	case ToolCallDelta:
		return "", "", 0
	default:
		return "", "", 0
	}
}

func (r *EventRenderer) logRender(ev Event, ch adapteropenai.StreamChunk) {
	kind := ev.eventKind()
	delta := adapteropenai.StreamDelta{}
	if len(ch.Choices) > 0 {
		delta = ch.Choices[0].Delta
	}
	attrs := []slog.Attr{
		slog.String("component", "adapter"),
		slog.String("subcomponent", "renderer"),
		slog.String("request_id", r.reqID),
		slog.String("backend", r.backend),
		slog.String("model", r.modelAlias),
		slog.String("alias", r.modelAlias),
		slog.String("event_kind", string(kind)),
		slog.String("render_policy", renderPolicyForEvent(kind)),
		slog.Int("delta_len", len(delta.Content)+len(delta.Reasoning)+len(delta.ReasoningContent)),
	}
	attrs = append(attrs, toolCallLogAttrs(delta.ToolCalls)...)
	r.log.LogAttrs(r.logContext(), slog.LevelDebug, "adapter.render.event", attrs...)
}

func shouldLogEvent(ev Event) bool {
	switch e := ev.(type) {
	case TextDelta, ReasoningDelta:
		return false
	case ToolCallDelta:
		return toolCallDeltaHasIdentity(e.ToolCalls)
	default:
		return true
	}
}

func toolCallDeltaHasIdentity(toolCalls []adapteropenai.ToolCall) bool {
	for _, tc := range toolCalls {
		if strings.TrimSpace(tc.ID) != "" || strings.TrimSpace(tc.Function.Name) != "" {
			return true
		}
	}
	return false
}

func toolCallLogAttrs(toolCalls []adapteropenai.ToolCall) []slog.Attr {
	if len(toolCalls) == 0 {
		return nil
	}
	names := make([]string, 0, len(toolCalls))
	ids := make([]string, 0, len(toolCalls))
	argChars := 0
	for _, tc := range toolCalls {
		appendUniqueLogValue(&names, tc.Function.Name, 4)
		appendUniqueLogValue(&ids, tc.ID, 4)
		argChars += len(tc.Function.Arguments)
	}
	return []slog.Attr{
		slog.Int("tool_call_count", len(toolCalls)),
		slog.Any("tool_call_names", names),
		slog.Any("tool_call_ids", ids),
		slog.Int("tool_call_arg_chars", argChars),
	}
}

func appendUniqueLogValue(values *[]string, value string, limit int) {
	value = strings.TrimSpace(value)
	if value == "" || limit <= 0 || len(*values) >= limit {
		return
	}
	if slices.Contains(*values, value) {
		return
	}
	*values = append(*values, value)
}

func (r *EventRenderer) recordSuppressedEvent(ev Event) {
	if r.suppressed == nil {
		r.suppressed = make(map[EventKind]*deltaSummary)
	}
	kind := ev.eventKind()
	summary := r.suppressed[kind]
	if summary == nil {
		summary = &deltaSummary{}
		r.suppressed[kind] = summary
	}
	summary.Count++
	chars := eventTextLen(ev)
	if td, ok := ev.(ToolCallDelta); ok {
		for _, tc := range td.ToolCalls {
			summary.ToolCalls++
			chars += len(tc.Function.Arguments)
			summary.ToolArgChars += len(tc.Function.Arguments)
		}
	}
	summary.Chars += chars
	if chars > summary.MaxChars {
		summary.MaxChars = chars
	}
}

// eventTextLen returns the text byte length of an event for suppression
// accounting.
func eventTextLen(ev Event) int {
	switch e := ev.(type) {
	case TextDelta:
		return len(e.Text)
	case RefusalDelta:
		return len(e.Text)
	case ReasoningDelta:
		return len(e.Text)
	default:
		return 0
	}
}

func (r *EventRenderer) flushSuppressedEventSummaries(ctx context.Context) {
	if len(r.suppressed) == 0 {
		return
	}
	for _, kind := range []EventKind{EventAssistantTextDelta, EventReasoningDelta, EventToolCallDelta} {
		summary := r.suppressed[kind]
		if summary == nil || summary.Count == 0 {
			continue
		}
		r.log.LogAttrs(ctx, slog.LevelDebug, "adapter.event.delta_summary",
			slog.String("component", "adapter"),
			slog.String("subcomponent", "renderer"),
			slog.String("request_id", r.reqID),
			slog.String("backend", r.backend),
			slog.String("model", r.modelAlias),
			slog.String("alias", r.modelAlias),
			slog.String("event_kind", string(kind)),
			slog.Int("delta_count", summary.Count),
			slog.Int("delta_chars", summary.Chars),
			slog.Int("max_delta_chars", summary.MaxChars),
			slog.Int("tool_call_count", summary.ToolCalls),
			slog.Int("tool_call_arg_chars", summary.ToolArgChars),
		)
		delete(r.suppressed, kind)
	}
	if len(r.suppressed) == 0 {
		r.suppressed = nil
	}
}

func (r *EventRenderer) logContext() context.Context {
	if r == nil || r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

func renderPolicyForEvent(kind EventKind) string {
	switch kind {
	case EventReasoningDelta, EventReasoningFinished, EventReasoningSignaled:
		return "thinking_inline"
	case EventToolCallDelta:
		return "tool_call_delta"
	default:
		return "content_delta"
	}
}
