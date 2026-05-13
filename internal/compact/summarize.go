package compact

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
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

// SummarizeOptions configures a claude -p summarization call.
type SummarizeOptions struct {
	// Binary is the path to the claude CLI. Defaults to "claude" on
	// $PATH when empty.
	Binary string

	// Model passed to claude via --model. Empty leaves claude's default.
	Model string

	// MaxInputBytes caps how much dropped-portion text is sent to the
	// summarizer. Trims from the oldest end (so most recent dropped
	// content survives). Default 200_000 bytes (~50k tokens).
	MaxInputBytes int

	// Timeout caps the subprocess. Default 120s.
	Timeout time.Duration
}

// SummarizeRequest is the generic input passed to summarize adapters.
type SummarizeRequest struct {
	Session *session.Session
	Slice   *Slice
	Options SynthOptions
	Model   string
	Mode    SummarizeMode
	Adapter SummarizeAdapter
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

// ClaudeSummarizeAdapter summarizes dropped content by calling the Claude CLI.
type ClaudeSummarizeAdapter struct {
	Options SummarizeOptions
}

// SummarizeDropped summarizes the dropped content for a Claude-owned compact
// run.
func (a ClaudeSummarizeAdapter) SummarizeDropped(ctx context.Context, req SummarizeRequest) (string, error) {
	options := a.Options
	if options.Model == "" {
		options.Model = req.Model
	}
	return SummarizeDropped(ctx, req.Slice, req.Options, options)
}

// SummarizeAdapterForSession returns the provider adapter for a session.
func SummarizeAdapterForSession(sess *session.Session) (SummarizeAdapter, error) {
	if sess == nil {
		return nil, fmt.Errorf("summarize adapter: nil session")
	}
	switch sess.ProviderID() {
	case session.ProviderClaude:
		return ClaudeSummarizeAdapter{
			Options: SummarizeOptions{
				Binary:        "",
				Model:         "",
				MaxInputBytes: 0,
				Timeout:       0,
			},
		}, nil
	case session.ProviderUnknown, session.ProviderCodex:
		return nil, fmt.Errorf("summarize adapter: provider %q is not supported", sess.ProviderID())
	default:
		return nil, fmt.Errorf("summarize adapter: provider %q is not supported", sess.ProviderID())
	}
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

// SummarizeDropped renders the portion of the slice that the current
// SynthOptions will drop, hands it to `claude -p` for summarization,
// and returns the summary text. Uses OAuth (no API key required).
// The summary is intended to be placed into SynthOptions.Summary
// before the final Synthesize call.
//
// Returns an empty string (not an error) when nothing is being
// dropped; callers can always invoke this unconditionally.
func SummarizeDropped(ctx context.Context, slice *Slice, opts SynthOptions, sopts SummarizeOptions) (string, error) {
	droppedText := renderDroppedForSummary(slice, opts)
	if strings.TrimSpace(droppedText) == "" {
		return "", nil
	}

	binary := sopts.Binary
	if binary == "" {
		binary = "claude"
	}
	maxBytes := sopts.MaxInputBytes
	if maxBytes <= 0 {
		maxBytes = 200_000
	}
	timeout := sopts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	if len(droppedText) > maxBytes {
		// Trim from the head so the most recent dropped content survives.
		excess := len(droppedText) - maxBytes
		droppedText = "[...earlier dropped content elided for length...]\n\n" + droppedText[excess:]
	}

	prompt := "The following is a transcript excerpt that is being removed from a long running agent conversation during context compaction. Write a concise recap (under 400 words) that preserves everything a continuing agent needs to know: user goals, decisions made, files touched, tools invoked, outcomes, and any in flight commitments. Use bullet points grouped by topic. Do not summarize the mechanics of the tools themselves; summarize what was accomplished. Do not greet the user. Output the recap only.\n\n" + droppedText

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p", "--no-session-persistence"}
	if sopts.Model != "" {
		args = append(args, "--model", sopts.Model)
	}
	args = append(args, prompt)

	started := compactClock.Now()
	compactLog.Logger().Info("compact.summarize.spawned",
		"component", "compact",
		"subcomponent", "summarize",
		"binary", binary,
		"prompt_bytes", len(prompt),
		"timeout_s", int(timeout.Seconds()),
	)

	cmd := exec.CommandContext(callCtx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 1024 {
			tail = tail[len(tail)-1024:]
		}
		slog.WarnContext(ctx, "compact.summarize.failed",
			"component", "compact",
			"subcomponent", "summarize",
			"duration_ms", time.Since(started).Milliseconds(),
			"stderr_tail", tail,
			"err", err,
		)
		return "", fmt.Errorf("summarize: %w", err)
	}
	summary := strings.TrimSpace(stdout.String())
	compactLog.Logger().Info("compact.summarize.completed",
		"component", "compact",
		"subcomponent", "summarize",
		"duration_ms", time.Since(started).Milliseconds(),
		"summary_bytes", len(summary),
	)
	return summary, nil
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
