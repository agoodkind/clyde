package parser

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	"goodkind.io/clyde/internal/transcript"
)

const (
	sourceTimesComponent = "source_times"
	toolKeySeparator     = "\x00"
)

var _ conversation.SourceStoreReader = (*Parser)(nil)

// SourceStoreMessages reads composer messages when the record has a supported native ID.
func (*Parser) SourceStoreMessages(ctx context.Context, record conversation.Record, opts conversation.LoadOptions) ([]transcript.Message, bool, error) {
	composerID := strings.TrimSpace(record.NativeID)
	if composerID == "" || record.ArtifactKind == artifactKindCursorLegacyChat {
		return nil, false, nil
	}
	return readComposerSourceMessages(ctx, composerID, opts)
}

// SourceMatchText normalizes stored text and removes a trailing turn outcome marker.
func (p *Parser) SourceMatchText(role string, text string) string {
	normalized := p.NormalizeStoredText(role, text)
	lastBreak := strings.LastIndexByte(normalized, '\n')
	lastLine := normalized[lastBreak+1:]
	isMarker := lastLine == turnBoundaryUnreadableMarker ||
		(strings.HasPrefix(lastLine, turnEndedMarkerPrefix) && strings.HasSuffix(lastLine, turnMarkerSuffix))
	if !isMarker {
		return normalized
	}
	if lastBreak < 0 {
		return ""
	}
	return normalized[:lastBreak]
}

// streamTranscript reads transcript messages, applies source times, and yields messages.
func streamTranscript(
	discovered discoveredArtifact,
	opts conversation.LoadOptions,
	yield func(transcript.Message, error) bool,
) {
	messages := make([]transcript.Message, 0)
	bodies := make([]string, 0)
	err := cursorjsonl.StreamMessages(discovered.Path, func(message cursorjsonl.TranscriptMessage) error {
		mapped, include := mapJSONLMessage(message, opts)
		if !include {
			return nil
		}
		messages = append(messages, mapped)
		bodies = append(bodies, transcriptBody(message, mapped))
		return nil
	})
	if err != nil {
		yield(emptyMessage(), err)
		return
	}
	applySourceTimes(context.Background(), discovered, opts, messages, bodies)
	for _, message := range messages {
		if !yield(message, nil) {
			return
		}
	}
}

// transcriptBody removes the turn outcome marker from mapped transcript text.
func transcriptBody(message cursorjsonl.TranscriptMessage, mapped transcript.Message) string {
	body := mapped.Text
	if marker := turnOutcomeMarker(message.Outcome); marker != "" {
		body = strings.TrimSuffix(strings.TrimSuffix(body, marker), "\n")
	}
	return body
}

// applySourceTimes fills missing message timestamps when the composer store has a unique match.
func applySourceTimes(
	ctx context.Context,
	discovered discoveredArtifact,
	opts conversation.LoadOptions,
	messages []transcript.Message,
	bodies []string,
) {
	undated := false
	for _, message := range messages {
		if message.Timestamp.IsZero() {
			undated = true
			break
		}
	}
	if !undated || discovered.ConversationID == "" {
		return
	}
	source, found, err := readComposerSourceMessages(ctx, discovered.ConversationID, opts)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_failed", "concern", concern, "path", discovered.Path, "composer_id", discovered.ConversationID, "err", err)
		return
	}
	if !found {
		return
	}
	texts, tools := newSourceTimeLookup(source)
	for position := range messages {
		if !messages[position].Timestamp.IsZero() {
			continue
		}
		if strings.TrimSpace(bodies[position]) != "" {
			if stamp, outcome := texts.TextTime(messages[position].Role, bodies[position]); outcome == transcript.SourceTimeFound {
				messages[position].Timestamp = stamp
			}
			continue
		}
		if len(messages[position].Tools) > 0 {
			if stamp, ok := uniqueSourceTime(tools[toolKey(messages[position].Role, messages[position].Tools[0])]); ok {
				messages[position].Timestamp = stamp
			}
		}
	}
}

// newSourceTimeLookup indexes source message text and tool calls by role.
func newSourceTimeLookup(source []transcript.Message) (*transcript.SourceTimeIndex, map[string][]time.Time) {
	entries := make([]transcript.SourceTimeEntry, 0, len(source))
	tools := make(map[string][]time.Time)
	for _, message := range source {
		entries = append(entries, transcript.SourceTimeEntry{Role: message.Role, Text: message.Text, Timestamp: message.Timestamp})
		for _, tool := range message.Tools {
			key := toolKey(message.Role, tool)
			tools[key] = append(tools[key], message.Timestamp)
		}
	}
	return transcript.NewSourceTimeIndex(entries), tools
}

func toolKey(role string, tool transcript.ToolCall) string {
	return strings.Join([]string{role, tool.Name, strings.TrimSpace(tool.Display)}, toolKeySeparator)
}

// uniqueSourceTime returns a timestamp when all candidates share one nonzero time.
func uniqueSourceTime(times []time.Time) (time.Time, bool) {
	if len(times) == 0 {
		return time.Time{}, false
	}
	for _, stamp := range times[1:] {
		if !stamp.Equal(times[0]) {
			return time.Time{}, false
		}
	}
	return times[0], !times[0].IsZero()
}

// readComposerSourceMessages reads the first composer found in the configured data roots.
func readComposerSourceMessages(ctx context.Context, composerID string, opts conversation.LoadOptions) ([]transcript.Message, bool, error) {
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_roots_failed", "concern", concern, "composer_id", composerID, "err", err)
		return nil, false, fmt.Errorf("resolve Cursor data roots: %w", err)
	}
	for _, root := range roots {
		db, availability := openOptionalDatabase(ctx, root.GlobalDBPath, sourceTimesComponent)
		switch availability {
		case databaseAvailabilityAbsent:
			continue
		case databaseAvailabilityUnreadable:
			return nil, false, fmt.Errorf("open Cursor global database %s", root.GlobalDBPath)
		case databaseAvailabilityOpen:
		}
		messages, found, readErr := composerSourceMessages(ctx, db, composerID, opts)
		closeErr := db.Close()
		if readErr != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.source_times_read_failed", "concern", concern, "path", root.GlobalDBPath, "composer_id", composerID, "err", readErr)
			return nil, false, fmt.Errorf("read composer from Cursor global database %s: %w", root.GlobalDBPath, readErr)
		}
		if closeErr != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.source_times_close_failed", "concern", concern, "path", root.GlobalDBPath, "err", closeErr)
			return nil, false, fmt.Errorf("close Cursor global database %s: %w", root.GlobalDBPath, closeErr)
		}
		if found {
			return messages, true, nil
		}
	}
	return nil, false, nil
}

func composerSourceMessages(ctx context.Context, db *sql.DB, composerID string, opts conversation.LoadOptions) ([]transcript.Message, bool, error) {
	header, found, err := cursorstore.ReadComposerHeader(ctx, db, composerID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_header_failed", "concern", concern, "composer_id", composerID, "err", err)
		return nil, false, fmt.Errorf("read composer header %q: %w", composerID, err)
	}
	if !found {
		return nil, false, nil
	}
	messages := make([]transcript.Message, 0)
	err = cursorstore.StreamComposerBubbles(ctx, db, composerID, header, func(bubble cursorstore.Bubble) bool {
		mapped, include := mapComposerBubble(bubble, opts)
		if include {
			messages = append(messages, mapped)
		}
		return true
	})
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_bubbles_failed", "concern", concern, "composer_id", composerID, "err", err)
		return nil, false, fmt.Errorf("read composer bubbles %q: %w", composerID, err)
	}
	return messages, true, nil
}
