package codex

import (
	"context"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
)

const (
	assistantDeltaPreviewRunes = 160
)

type sseInstrumentationContext struct {
	RequestID          string
	CursorRequestID    string
	ConversationID     string
	Correlation        correlation.Context
	Alias              string
	Model              string
	Transport          string
	ServiceTier        string
	PromptCacheKey     string
	PreviousResponseID string
	Warmup             bool
}

func (c sseInstrumentationContext) Enabled() bool {
	return c.RequestID != "" ||
		c.CursorRequestID != "" ||
		c.ConversationID != "" ||
		c.Alias != "" ||
		c.Model != "" ||
		c.Transport != "" ||
		c.ServiceTier != "" ||
		c.PromptCacheKey != "" ||
		c.PreviousResponseID != "" ||
		c.Correlation.TraceID != "" ||
		c.Correlation.SpanID != ""
}

type sseAggregateCollector struct {
	Assistant assistantTextDeltaAggregate
	Items     outputItemAggregate
	Tools     toolDeltaAggregate
	Reasoning reasoningAggregate
}

func (c *sseAggregateCollector) observe(eventName string, raw transportStreamEvent) {
	switch codexSSEEventName(eventName) {
	case codexSSEEventOutputTextDelta:
		c.Assistant.Add(raw.Delta)
	case codexSSEEventOutputItemAdded:
		c.Items.Added.Add(raw.Item)
	case codexSSEEventOutputItemDone:
		c.Items.Done.Add(raw.Item)
	case codexSSEEventFunctionCallArgsDelta:
		c.Tools.FunctionArgumentDeltaCount++
		c.Tools.FunctionArgumentDeltaChars += utf8.RuneCountInString(raw.Delta)
	case codexSSEEventCustomToolCallInputDelta:
		c.Tools.CustomToolInputDeltaCount++
		c.Tools.CustomToolInputDeltaChars += utf8.RuneCountInString(raw.Delta)
	default:
		if strings.Contains(eventName, "reasoning") && strings.HasSuffix(eventName, ".delta") {
			c.Reasoning.Add(eventName, raw.Delta)
		}
	}
}

func (c *sseAggregateCollector) Log(ctx context.Context, logCtx sseInstrumentationContext, responseID, status, errText string) {
	if logCtx.Transport == "" {
		logCtx.Transport = "responses_websocket"
	}
	corr := logCtx.Correlation
	if strings.TrimSpace(responseID) != "" {
		corr = clydeingress.WithUpstreamResponseID(corr, responseID)
	}
	attrs := []slog.Attr{
		slog.String("subcomponent", "codex"),
		slog.String("transport", logCtx.Transport),
		slog.Bool("warmup", logCtx.Warmup),
	}
	if logCtx.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", logCtx.RequestID))
	}
	if logCtx.CursorRequestID != "" {
		attrs = append(attrs, slog.String("cursor_request_id", logCtx.CursorRequestID))
	}
	if logCtx.ConversationID != "" {
		attrs = append(attrs, slog.String("conversation_id", logCtx.ConversationID))
	}
	attrs = correlation.AppendAttrs(attrs, corr)
	if logCtx.Alias != "" {
		attrs = append(attrs, slog.String("alias", logCtx.Alias))
	}
	if logCtx.Model != "" {
		attrs = append(attrs, slog.String("model", logCtx.Model))
	}
	if logCtx.ServiceTier != "" {
		attrs = append(attrs, slog.String("service_tier", logCtx.ServiceTier))
	}
	if logCtx.PromptCacheKey != "" {
		attrs = append(attrs, slog.String("prompt_cache_key", logCtx.PromptCacheKey))
	}
	if logCtx.PreviousResponseID != "" {
		attrs = append(attrs, slog.Bool("previous_response_id_present", true))
	}
	if status != "" {
		attrs = append(attrs, slog.String("response_status", status))
	}
	if errText != "" {
		attrs = append(attrs, slog.String("err", errText))
	}
	attrs = append(attrs, c.Assistant.toSlogAttrs()...)
	attrs = append(attrs, c.Items.toSlogAttrs()...)
	attrs = append(attrs, c.Tools.toSlogAttrs()...)
	attrs = append(attrs, c.Reasoning.toSlogAttrs()...)
	logCodexEventWithConcern(ctx, slog.LevelInfo, "adapter.codex.upstream_sse.aggregate", slogger.ConcernAdapterProviderCodexWS, attrs)
}

type assistantTextDeltaAggregate struct {
	DeltaCount        int
	CharCount         int
	FirstPreview      string
	LastPreview       string
	NormalizedText    strings.Builder
	PendingWhitespace bool
}

func (a *assistantTextDeltaAggregate) Add(delta string) {
	if delta == "" {
		return
	}
	a.DeltaCount++
	a.CharCount += utf8.RuneCountInString(delta)
	if a.FirstPreview == "" {
		a.FirstPreview = boundedPreview(delta)
	}
	a.LastPreview = boundedPreview(delta)
	a.appendText(delta)
}

func (a *assistantTextDeltaAggregate) appendText(delta string) {
	for _, r := range delta {
		if unicode.IsSpace(r) {
			a.PendingWhitespace = true
			continue
		}
		if a.PendingWhitespace && a.NormalizedText.Len() > 0 {
			a.NormalizedText.WriteByte(' ')
		}
		a.PendingWhitespace = false
		a.NormalizedText.WriteRune(r)
	}
}

func (a *assistantTextDeltaAggregate) toSlogAttrs() []slog.Attr {
	normalized := a.NormalizedText.String()
	repeated := detectRepeatedAssistantText(normalized)
	attrs := []slog.Attr{
		slog.Int("assistant_text_delta_count", a.DeltaCount),
		slog.Int("assistant_text_delta_chars", a.CharCount),
		slog.Int("assistant_text_normalized_chars", utf8.RuneCountInString(normalized)),
		slog.String("assistant_text_normalized_sha256", sha256StringHex(normalized)),
		slog.Bool("assistant_text_repeated_block_detected", repeated.Detected),
		slog.Int("assistant_text_repeated_block_chars", repeated.BlockChars),
		slog.Int("assistant_text_repeated_block_count", repeated.BlockCount),
		slog.Int("assistant_text_repeated_prefix_suffix_chars", repeated.PrefixSuffixChars),
	}
	if a.FirstPreview != "" {
		attrs = append(attrs, slog.String("assistant_text_first_preview", a.FirstPreview))
	}
	if a.LastPreview != "" {
		attrs = append(attrs, slog.String("assistant_text_last_preview", a.LastPreview))
	}
	if repeated.BlockSHA256 != "" {
		attrs = append(attrs, slog.String("assistant_text_repeated_block_sha256", repeated.BlockSHA256))
	}
	if repeated.BlockPreview != "" {
		attrs = append(attrs, slog.String("assistant_text_repeated_block_preview", repeated.BlockPreview))
	}
	return attrs
}

type assistantRepeatedText struct {
	Detected          bool
	BlockCount        int
	BlockChars        int
	BlockSHA256       string
	BlockPreview      string
	PrefixSuffixChars int
}

func detectRepeatedAssistantText(text string) assistantRepeatedText {
	normalized := normalizeAssistantText(text)
	if normalized == "" {
		return assistantRepeatedText{Detected: false, BlockCount: 0, BlockChars: 0, BlockSHA256: "", BlockPreview: "", PrefixSuffixChars: 0}
	}
	prefixSuffixChars := repeatedPrefixSuffixChars(normalized)
	runes := []rune(normalized)
	if len(runes)%2 == 1 {
		mid := len(runes) / 2
		if runes[mid] == ' ' && string(runes[:mid]) == string(runes[mid+1:]) {
			block := string(runes[:mid])
			return assistantRepeatedText{
				Detected:          true,
				BlockCount:        2,
				BlockChars:        mid,
				BlockSHA256:       sha256StringHex(block),
				BlockPreview:      boundedPreview(block),
				PrefixSuffixChars: prefixSuffixChars,
			}
		}
	}
	for blockLen := 1; blockLen <= len(runes)/2; blockLen++ {
		if len(runes)%blockLen != 0 {
			continue
		}
		count := len(runes) / blockLen
		if count < 2 {
			continue
		}
		block := string(runes[:blockLen])
		ok := true
		for start := blockLen; start < len(runes); start += blockLen {
			if string(runes[start:start+blockLen]) != block {
				ok = false
				break
			}
		}
		if ok {
			return assistantRepeatedText{
				Detected:          true,
				BlockCount:        count,
				BlockChars:        blockLen,
				BlockSHA256:       sha256StringHex(block),
				BlockPreview:      boundedPreview(block),
				PrefixSuffixChars: prefixSuffixChars,
			}
		}
	}
	return assistantRepeatedText{PrefixSuffixChars: prefixSuffixChars, Detected: false, BlockCount: 0, BlockChars: 0, BlockSHA256: "", BlockPreview: ""}
}

func repeatedPrefixSuffixChars(text string) int {
	runes := []rune(text)
	for size := len(runes) / 2; size > 0; size-- {
		if string(runes[:size]) == string(runes[len(runes)-size:]) {
			return size
		}
	}
	return 0
}

type outputItemAggregate struct {
	Added outputItemCounts
	Done  outputItemCounts
}

func (a outputItemAggregate) toSlogAttrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("output_item_added_count", a.Added.Total),
		slog.Int("output_item_done_count", a.Done.Total),
		slog.Int("output_item_tool_added_count", a.Added.ToolTotal),
		slog.Int("output_item_tool_done_count", a.Done.ToolTotal),
	}
	if len(a.Added.TypeCounts) > 0 {
		attrs = append(attrs, slog.Attr{Key: "output_item_added_type_counts", Value: slog.GroupValue(intMapAttrs(a.Added.TypeCounts)...)})
	}
	if len(a.Done.TypeCounts) > 0 {
		attrs = append(attrs, slog.Attr{Key: "output_item_done_type_counts", Value: slog.GroupValue(intMapAttrs(a.Done.TypeCounts)...)})
	}
	return attrs
}

type outputItemCounts struct {
	Total      int
	ToolTotal  int
	TypeCounts map[string]int
}

func (c *outputItemCounts) Add(item transportItem) {
	c.Total++
	itemType := strings.TrimSpace(item.string("type"))
	if itemType == "" {
		itemType = "unknown"
	}
	if c.TypeCounts == nil {
		c.TypeCounts = make(map[string]int)
	}
	c.TypeCounts[itemType]++
	switch codexItemType(itemType) {
	case codexItemTypeFunctionCall, codexItemTypeLocalShellCall, codexItemTypeCustomToolCall:
		c.ToolTotal++
	case codexItemTypeMessage, codexItemTypeFunctionCallOutput, codexItemTypeCustomToolCallOutput, codexItemTypeReasoning:
		// Non-tool items don't bump ToolTotal; explicit cases keep
		// the switch exhaustive for the codexItemType enum.
	}
}

// codexSSEEventName enumerates the SSE event names the codex
// aggregate collector recognizes.
type codexSSEEventName string

const (
	codexSSEEventOutputTextDelta          codexSSEEventName = "response.output_text.delta"
	codexSSEEventOutputItemAdded          codexSSEEventName = "response.output_item.added"
	codexSSEEventOutputItemDone           codexSSEEventName = "response.output_item.done"
	codexSSEEventFunctionCallArgsDelta    codexSSEEventName = "response.function_call_arguments.delta"
	codexSSEEventCustomToolCallInputDelta codexSSEEventName = "response.custom_tool_call_input.delta"
)

type toolDeltaAggregate struct {
	FunctionArgumentDeltaCount int
	FunctionArgumentDeltaChars int
	CustomToolInputDeltaCount  int
	CustomToolInputDeltaChars  int
}

func (a toolDeltaAggregate) toSlogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("tool_function_argument_delta_count", a.FunctionArgumentDeltaCount),
		slog.Int("tool_function_argument_delta_chars", a.FunctionArgumentDeltaChars),
		slog.Int("tool_custom_input_delta_count", a.CustomToolInputDeltaCount),
		slog.Int("tool_custom_input_delta_chars", a.CustomToolInputDeltaChars),
	}
}

type reasoningAggregate struct {
	DeltaCount        int
	DeltaChars        int
	TextDeltaCount    int
	TextDeltaChars    int
	SummaryDeltaCount int
	SummaryDeltaChars int
}

func (a *reasoningAggregate) Add(eventName, delta string) {
	if delta == "" {
		return
	}
	chars := utf8.RuneCountInString(delta)
	a.DeltaCount++
	a.DeltaChars += chars
	if strings.Contains(eventName, "summary") {
		a.SummaryDeltaCount++
		a.SummaryDeltaChars += chars
		return
	}
	a.TextDeltaCount++
	a.TextDeltaChars += chars
}

func (a *reasoningAggregate) toSlogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("reasoning_delta_count", a.DeltaCount),
		slog.Int("reasoning_delta_chars", a.DeltaChars),
		slog.Int("reasoning_text_delta_count", a.TextDeltaCount),
		slog.Int("reasoning_text_delta_chars", a.TextDeltaChars),
		slog.Int("reasoning_summary_delta_count", a.SummaryDeltaCount),
		slog.Int("reasoning_summary_delta_chars", a.SummaryDeltaChars),
	}
}

func normalizeAssistantText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func boundedPreview(text string) string {
	normalized := normalizeAssistantText(text)
	if normalized == "" {
		return ""
	}
	runes := []rune(normalized)
	if len(runes) <= assistantDeltaPreviewRunes {
		return normalized
	}
	return string(runes[:assistantDeltaPreviewRunes])
}
