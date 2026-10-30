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
	rootHash := ""
	if strings.HasPrefix(record.ArtifactPath, virtualPathPrefix) {
		virtualPath, err := ParseVirtualPath(record.ArtifactPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.source_times_virtual_path_failed", "concern", concern, "path", record.ArtifactPath, "err", err)
			return nil, false, fmt.Errorf("parse cursor data root from record path %s: %w", record.ArtifactPath, err)
		}
		rootHash = virtualPath.RootHash
	}
	return readComposerSourceMessages(ctx, composerID, rootHash, opts)
}

// SourceMatchText normalizes stored text and removes a trailing turn outcome marker.
func (p *Parser) SourceMatchText(role string, text string) string {
	normalized := p.NormalizeStoredText(role, text)
	if role != cursorjsonl.RoleAssistant {
		return normalized
	}
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

// sourceToolAliases maps Cursor store tool names to transcript tool names.
// Cursor 3.27.18 maps run_terminal_command_v2 to Shell.
var sourceToolAliases = map[string]string{
	"run_terminal_command_v2": "Shell",
}

// SourceToolName returns the transcript name for a Cursor store tool name.
// It returns the original name when the store name has no alias.
func (*Parser) SourceToolName(name string) string {
	return sourceToolName(name)
}

func sourceToolName(name string) string {
	if alias, found := sourceToolAliases[name]; found {
		return alias
	}
	return name
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
	source, found, err := readComposerSourceMessages(ctx, discovered.ConversationID, "", opts)
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
			tool.Name = sourceToolName(tool.Name)
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

// readComposerSourceMessages reads a composer from the data root matching
// rootHash when rootHash is not empty. When rootHash is empty, it reads the
// composer from every configured data root that contains it. It returns an error
// when two roots return different messages or times. It reports found as false
// when no root contains the composer.
func readComposerSourceMessages(ctx context.Context, composerID string, rootHash string, opts conversation.LoadOptions) ([]transcript.Message, bool, error) {
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_roots_failed", "concern", concern, "composer_id", composerID, "err", err)
		return nil, false, fmt.Errorf("resolve Cursor data roots: %w", err)
	}
	var chosen []transcript.Message
	chosenPath := ""
	for _, root := range roots {
		if rootHash != "" && RootHash(root.RootDir) != rootHash {
			continue
		}
		messages, found, readErr := rootComposerSourceMessages(ctx, root.GlobalDBPath, composerID, opts)
		if readErr != nil {
			return nil, false, readErr
		}
		if !found {
			continue
		}
		if chosenPath == "" {
			chosen = messages
			chosenPath = root.GlobalDBPath
			continue
		}
		if sourceSignature(messages) != sourceSignature(chosen) {
			conflictErr := fmt.Errorf("cursor composer %q has different messages or times in global databases %s and %s", composerID, chosenPath, root.GlobalDBPath)
			slog.WarnContext(ctx, "providers.cursor.parser.source_times_roots_conflict", "concern", concern, "composer_id", composerID, "first", chosenPath, "second", root.GlobalDBPath, "err", conflictErr)
			return nil, false, conflictErr
		}
	}
	return chosen, chosenPath != "", nil
}

// rootComposerSourceMessages reads a composer's messages from the global
// database at path. It reports found as false when the database or composer
// is absent.
func rootComposerSourceMessages(ctx context.Context, path string, composerID string, opts conversation.LoadOptions) ([]transcript.Message, bool, error) {
	db, availability := openOptionalDatabase(ctx, path, sourceTimesComponent)
	switch availability {
	case databaseAvailabilityAbsent:
		return nil, false, nil
	case databaseAvailabilityUnreadable:
		return nil, false, fmt.Errorf("open Cursor global database %s", path)
	case databaseAvailabilityOpen:
	}
	messages, found, readErr := composerSourceMessages(ctx, db, composerID, opts)
	closeErr := db.Close()
	if readErr != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_read_failed", "concern", concern, "path", path, "composer_id", composerID, "err", readErr)
		return nil, false, fmt.Errorf("read composer from Cursor global database %s: %w", path, readErr)
	}
	if closeErr != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_close_failed", "concern", concern, "path", path, "err", closeErr)
		return nil, false, fmt.Errorf("close Cursor global database %s: %w", path, closeErr)
	}
	return messages, found, nil
}

// sourceSignature returns a string built from each message's role, text,
// reasoning, time, and tool calls for comparison between roots.
func sourceSignature(messages []transcript.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		builder.WriteString(message.Role + toolKeySeparator + message.Text + toolKeySeparator + message.Thinking + toolKeySeparator + message.Timestamp.UTC().Format(time.RFC3339Nano))
		for _, tool := range message.Tools {
			builder.WriteString(toolKeySeparator + toolKey(message.Role, tool))
		}
		builder.WriteString("\n")
	}
	return builder.String()
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
