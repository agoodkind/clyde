package parser

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/transcript"
)

// resolveLegacyTranscripts opens the transcript again and attaches typed spans
// to compact summary messages with unframed wrappers that the transcript's
// provenance reproduces.
func resolveLegacyTranscripts(path string, index *provenanceIndex, messages []transcript.Message, opts parseOptions) error {
	if len(index.summaries) == 0 {
		return nil
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		slog.Warn("providers.claude.parser.legacy_transcript_open_failed", "concern", concern, "component", "claude", "path", path, "err", err)
		return fmt.Errorf("open transcript for legacy transcript provenance: %w", err)
	}
	defer func() { _ = file.Close() }()
	resolver := newLegacyResolver(newProvenanceReader(file, index))
	for messageIndex := range messages {
		resolver.applyToMessage(&messages[messageIndex], opts)
	}
	return nil
}

// applyToMessage resolves a summary message with an unframed wrapper and
// without a framed span. On failure, it logs the summary and unresolved
// references and leaves the message unchanged. A filtered export of that
// summary then fails.
func (resolver *legacyResolver) applyToMessage(message *transcript.Message, opts parseOptions) {
	if message.Compaction == nil || message.Compaction.Kind != transcript.CompactionKindSummary {
		return
	}
	if _, ok := resolver.reader.index.summaries[message.UUID]; !ok {
		return
	}
	content, err := resolver.summaryContent(message.UUID)
	if err != nil {
		slog.Warn("providers.claude.parser.legacy_summary_read_failed",
			"concern", concern, "component", "claude", "summary_uuid", message.UUID, "err", err)
		return
	}
	if len(reorienttag.CompleteTranscriptSpans(content)) > 0 || !reorienttag.ContainsTranscriptWrapper(content) {
		return
	}
	resolution, err := resolver.resolveSummary(message.UUID)
	if err != nil {
		slog.Warn("providers.claude.parser.legacy_transcript_unresolved",
			"concern", concern, "component", "claude", "summary_uuid", message.UUID, "err", err)
		return
	}
	report := resolution.report
	slog.Debug("providers.claude.parser.legacy_transcript_resolved",
		"concern", concern,
		"component", "claude",
		"summary_uuid", message.UUID,
		"request_id", report.requestID,
		"summaries", report.summaries,
		"request_positions", report.positions,
		"direct_blobs", report.directBlobs,
		"normalized_blobs", report.normalizedBlobs,
		"retained_positions", report.retainedPositions,
		"segments", resolution.segments,
	)
	applyLegacySpan(message, content, resolution.span, opts)
}

// applyLegacySpan rebuilds summary text using user text stripping rules
// outside the wrapper and preserving wrapper bytes. It attaches the span with
// offsets in that text and applies user chat stripping rules to user chat
// segments. When the selection includes every content kind, the text equals
// the trimmed original.
func applyLegacySpan(message *transcript.Message, content string, span reorienttag.TranscriptSpan, opts parseOptions) {
	opts.Tally = nil
	wrapper := content[span.Start:span.End]
	before := legacyProse(content[:span.Start], opts, true)
	after := legacyProse(content[span.End:], opts, false)
	full := before + wrapper + after
	leading := len(full) - len(strings.TrimLeftFunc(full, unicode.IsSpace))
	start := len(before) - leading
	text := strings.TrimSpace(full)
	resolved := reorienttag.TranscriptSpan{
		Start:    start,
		End:      start + len(wrapper),
		Segments: stripSummarySegments(span.Segments, opts),
	}
	message.Text = text
	for itemIndex := range message.Compaction.ContextItems {
		item := &message.Compaction.ContextItems[itemIndex]
		if item.Message == nil || item.Message.MessageClass != transcript.CompactedMessageClassSummary || len(item.Message.Content) == 0 {
			continue
		}
		item.Message.Content[0].Text = text
		item.Message.Content[0].TranscriptSpans = []reorienttag.TranscriptSpan{resolved}
	}
}

// legacyProse returns text unchanged when user text stripping changes only
// surrounding whitespace. Otherwise, it returns stripped text with a blank
// line between that text and the wrapper.
func legacyProse(text string, opts parseOptions, leading bool) string {
	stripped := strings.TrimSpace(stripUserText(text, opts))
	if stripped == strings.TrimSpace(text) {
		return text
	}
	if stripped == "" {
		return ""
	}
	if leading {
		return stripped + summaryPieceSeparator
	}
	return summaryPieceSeparator + stripped
}
