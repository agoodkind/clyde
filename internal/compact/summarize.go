package compact

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/session"
)

// SummarizeMode controls whether compact injects a generated recap of dropped
// content.
type SummarizeMode string

const (
	// SummarizeModeAuto summarizes only when chat content was truncated.
	SummarizeModeAuto SummarizeMode = "auto"
	// SummarizeModeOn summarizes any dropped content.
	SummarizeModeOn SummarizeMode = "on"
	// SummarizeModeOff disables summarization.
	SummarizeModeOff SummarizeMode = "off"
)

// NormalizeSummarizeMode maps wire/UI/CLI values onto the compact
// summarization policy. Empty values preserve the new default: summarize only
// when chat turns were truncated.
func NormalizeSummarizeMode(value string) (SummarizeMode, error) {
	switch SummarizeMode(strings.ToLower(strings.TrimSpace(value))) {
	case "", SummarizeModeAuto:
		return SummarizeModeAuto, nil
	case SummarizeModeOn:
		return SummarizeModeOn, nil
	case SummarizeModeOff:
		return SummarizeModeOff, nil
	default:
		return "", fmt.Errorf("unknown summarize mode %q (expected auto|on|off)", value)
	}
}

// SummarizeModeFromLegacy maps the old boolean summarize setting onto a mode.
func SummarizeModeFromLegacy(summarize bool, explicit bool) SummarizeMode {
	if !explicit {
		return SummarizeModeAuto
	}
	if summarize {
		return SummarizeModeOn
	}
	return SummarizeModeOff
}

// SummarizeRequest is the generic input passed to summarize adapters.
type SummarizeRequest struct {
	Session     *session.Session
	Slice       *Slice
	Options     SynthOptions
	Model       string
	Mode        SummarizeMode
	Adapter     SummarizeAdapter
	DroppedText string
}

// SummarizeDecision records why a compact run did or did not summarize
// content.
type SummarizeDecision struct {
	Mode            SummarizeMode
	ShouldSummarize bool
	Reason          string
	Stats           DroppedStats
	Summary         string
}

// SummarizeAdapter provides provider-specific dropped-content summarization.
type SummarizeAdapter interface {
	SummarizeDropped(ctx context.Context, req SummarizeRequest) (string, error)
}

// SummarizeAdapterFactory constructs a provider-owned summarize adapter.
type SummarizeAdapterFactory func() SummarizeAdapter

var summarizeAdapters = struct {
	mu        sync.RWMutex
	factories map[session.ProviderID]SummarizeAdapterFactory
}{
	mu:        sync.RWMutex{},
	factories: map[session.ProviderID]SummarizeAdapterFactory{},
}

// RegisterSummarizeAdapter adds a provider-owned summarize adapter factory.
func RegisterSummarizeAdapter(provider session.ProviderID, factory SummarizeAdapterFactory) error {
	normalizedProvider := session.NormalizeProviderID(provider)
	if factory == nil {
		return fmt.Errorf("summarize adapter factory is nil: %s", normalizedProvider)
	}
	summarizeAdapters.mu.Lock()
	defer summarizeAdapters.mu.Unlock()
	if _, exists := summarizeAdapters.factories[normalizedProvider]; exists {
		return fmt.Errorf("summarize adapter already registered: %s", normalizedProvider)
	}
	summarizeAdapters.factories[normalizedProvider] = factory
	return nil
}

// SummarizeAdapterForSession returns the registered adapter for a session.
func SummarizeAdapterForSession(sess *session.Session) (SummarizeAdapter, error) {
	if sess == nil {
		return nil, fmt.Errorf("summarize adapter: nil session")
	}
	provider := session.NormalizeProviderID(sess.ProviderID())
	summarizeAdapters.mu.RLock()
	factory, ok := summarizeAdapters.factories[provider]
	summarizeAdapters.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("summarize adapter: provider %q is not supported", sess.ProviderID())
	}
	adapter := factory()
	if adapter == nil {
		return nil, fmt.Errorf("summarize adapter: provider %q returned nil adapter", sess.ProviderID())
	}
	return adapter, nil
}

// DoSummarize decides whether to summarize and calls the matching provider
// adapter.
func DoSummarize(ctx context.Context, req SummarizeRequest) (SummarizeDecision, error) {
	mode := req.Mode
	if mode == "" {
		mode = SummarizeModeAuto
	}
	stats := ComputeDroppedStats(req.Slice, req.Options)
	decision := SummarizeDecision{
		Mode:            mode,
		ShouldSummarize: shouldSummarize(mode, stats, req.Options),
		Reason:          summarizeReason(mode, stats, req.Options),
		Stats:           stats,
		Summary:         "",
	}
	if !decision.ShouldSummarize {
		return decision, nil
	}
	req.DroppedText = renderDroppedForSummary(req.Slice, req.Options)
	adapter := req.Adapter
	if adapter == nil {
		resolved, err := SummarizeAdapterForSession(req.Session)
		if err != nil {
			return decision, err
		}
		adapter = resolved
	}
	summary, err := adapter.SummarizeDropped(ctx, req)
	if err != nil {
		return decision, fmt.Errorf("summarize dropped content: %w", err)
	}
	decision.Summary = summary
	return decision, nil
}

func shouldSummarize(mode SummarizeMode, stats DroppedStats, opts SynthOptions) bool {
	switch mode {
	case SummarizeModeOff:
		return false
	case SummarizeModeOn:
		return hasDroppedContent(stats, opts)
	case SummarizeModeAuto:
		return chatWasTruncated(stats, opts)
	default:
		return chatWasTruncated(stats, opts)
	}
}

func summarizeReason(mode SummarizeMode, stats DroppedStats, opts SynthOptions) string {
	switch mode {
	case SummarizeModeOff:
		return "disabled"
	case SummarizeModeOn:
		if hasDroppedContent(stats, opts) {
			return "enabled"
		}
		return "no dropped content"
	case SummarizeModeAuto:
		if chatWasTruncated(stats, opts) {
			return "chat truncated"
		}
		return "no chat truncation"
	default:
		if chatWasTruncated(stats, opts) {
			return "chat truncated"
		}
		return "no chat truncation"
	}
}

func chatWasTruncated(stats DroppedStats, opts SynthOptions) bool {
	return stats.ChatTurns > 0 || droppedSummaryChunkCount(opts) > 0
}

func hasDroppedContent(stats DroppedStats, opts SynthOptions) bool {
	return stats.ThinkingBlocks+stats.Images+stats.ToolsLineOnly+stats.ToolsDropped+stats.ChatTurns+droppedSummaryChunkCount(opts) > 0
}

// renderDroppedForSummary emits a plain text view of just the
// portions the current opts will drop from the slice. This is what we
// feed to the summarizer.
func renderDroppedForSummary(slice *Slice, opts SynthOptions) string {
	var sb strings.Builder
	renderDroppedChatTurns(&sb, slice, opts)
	renderDroppedSummaryChunks(&sb, slice, opts)
	renderDroppedToolCalls(&sb, slice, opts)
	return sb.String()
}

func renderDroppedChatTurns(sb *strings.Builder, slice *Slice, opts SynthOptions) {
	if len(opts.DroppedChatEntries) > 0 {
		sb.WriteString("# Dropped chat turns\n\n")
		for ei, e := range slice.PostBoundary {
			if !opts.DroppedChatEntries[ei] {
				continue
			}
			if e.Type != "user" && e.Type != "assistant" {
				continue
			}
			text := chatTextFrom(e, droppedChatTextOptions())
			if text == "" {
				continue
			}
			fmt.Fprintf(
				sb,
				"## %s (%s)\n\n%s\n\n",
				titleCaseASCII(e.Type),
				e.Timestamp.UTC().Format(time.RFC3339),
				text,
			)
		}
	}
}

func droppedChatTextOptions() SynthOptions {
	return SynthOptions{
		DropThinking:         true,
		ImagesAsPlaceholder:  false,
		ToolDefault:          ToolDetailFull,
		ToolDetailOverride:   nil,
		DroppedChatEntries:   nil,
		DroppedSummaryChunks: nil,
		TruncTokens:          0,
		Summary:              "",
	}
}

func renderDroppedSummaryChunks(sb *strings.Builder, slice *Slice, opts SynthOptions) {
	indexes := droppedSummaryIndexes(opts)
	wroteHeader := false
	for _, ei := range indexes {
		text := droppedSummaryText(slice, opts, ei)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if !wroteHeader {
			sb.WriteString("# Dropped prior compact summary chunks\n\n")
			wroteHeader = true
		}
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
}

func droppedSummaryIndexes(opts SynthOptions) []int {
	indexes := make([]int, 0, len(opts.DroppedSummaryChunks))
	for ei := range opts.DroppedSummaryChunks {
		indexes = append(indexes, ei)
	}
	sort.Ints(indexes)
	return indexes
}

func droppedSummaryText(slice *Slice, opts SynthOptions, entryIndex int) string {
	dropped := opts.DroppedSummaryChunks[entryIndex]
	if len(dropped) == 0 {
		return ""
	}
	if entryIndex < 0 || entryIndex >= len(slice.PostBoundary) {
		return ""
	}
	return droppedSyntheticSummaryText(slice.PostBoundary[entryIndex], dropped)
}

type droppedToolCall struct {
	Block ContentBlock
	Entry Entry
}

func renderDroppedToolCalls(sb *strings.Builder, slice *Slice, opts SynthOptions) {
	droppedTools := droppedToolCalls(slice, opts)
	if len(droppedTools) == 0 {
		return
	}
	sb.WriteString("# Dropped tool calls\n\n")
	for _, toolCall := range droppedTools {
		args := summarizeToolArgs(toolCall.Block)
		fmt.Fprintf(
			sb,
			"- %s %s(%s)\n",
			toolCall.Entry.Timestamp.UTC().Format(time.RFC3339),
			toolCall.Block.ToolName,
			args,
		)
	}
	sb.WriteString("\n")
}

func droppedToolCalls(slice *Slice, opts SynthOptions) []droppedToolCall {
	var droppedTools []droppedToolCall
	for _, e := range slice.PostBoundary {
		if e.Type != "assistant" {
			continue
		}
		for _, b := range e.Content {
			if b.Type != "tool_use" {
				continue
			}
			detail := opts.ToolDefault
			if override, ok := opts.ToolDetailOverride[b.ToolUseID]; ok {
				detail = override
			}
			if detail == ToolDetailDrop {
				droppedTools = append(droppedTools, droppedToolCall{
					Block: b,
					Entry: e,
				})
			}
		}
	}
	return droppedTools
}

// titleCaseASCII upper-cases the first byte of an ASCII identifier such as
// "user" or "assistant". It exists to avoid the deprecated [strings.Title]
// (deprecated since Go 1.18 due to Unicode word-boundary issues) for inputs
// that are guaranteed lowercase ASCII.
func titleCaseASCII(s string) string {
	if s == "" {
		return s
	}
	c := s[0]
	if c >= 'a' && c <= 'z' {
		return string(c-32) + s[1:]
	}
	return s
}
