package parser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	"goodkind.io/clyde/internal/transcript"
)

const (
	sourceTimesComponent     = "source_times"
	sourceSignatureSeparator = "\x00"
	// SourceToolReasonIdentityAbsent indicates that a call lacks a dated identity bubble.
	SourceToolReasonIdentityAbsent = "identity_bubble_absent"
	// SourceToolReasonIdentityConflict indicates that identity bubble dates disagree.
	SourceToolReasonIdentityConflict = "identity_dates_conflict"
	sourceCopiesMarker               = "copies"
	sourceToolsMarker                = "tools"
)

var _ conversation.SourceStoreReader = (*Parser)(nil)

// SourceStoreMessages reads composer messages when the record has a supported native ID.
func (*Parser) SourceStoreMessages(ctx context.Context, record conversation.Record, opts conversation.LoadOptions) (conversation.SourceMessages, bool, error) {
	composerID := strings.TrimSpace(record.NativeID)
	if composerID == "" || record.ArtifactKind == artifactKindCursorLegacyChat {
		return emptySourceMessages(), false, nil
	}
	rootHash := ""
	if strings.HasPrefix(record.ArtifactPath, virtualPathPrefix) {
		virtualPath, err := ParseVirtualPath(record.ArtifactPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.source_times_virtual_path_failed", "concern", concern, "path", record.ArtifactPath, "err", err)
			return emptySourceMessages(), false, fmt.Errorf("parse cursor data root from record path %s: %w", record.ArtifactPath, err)
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

func emptySourceMessages() conversation.SourceMessages {
	return conversation.SourceMessages{Messages: nil, Copies: nil, ToolCalls: nil, ToolCallsComplete: false}
}

// streamTranscript reads transcript messages, applies source times, and yields messages.
func streamTranscript(
	discovered discoveredArtifact,
	opts conversation.LoadOptions,
	yield func(transcript.Message, error) bool,
) {
	resolver := newSourceTimeResolver(discovered, opts)
	err := cursorjsonl.StreamMessages(discovered.Path, func(message cursorjsonl.TranscriptMessage) error {
		mapped, include := mapJSONLMessage(message, opts)
		if !include {
			return nil
		}
		resolver.apply(&mapped, transcriptBody(message, mapped))
		if !yield(mapped, nil) {
			return cursorjsonl.ErrStopStreaming
		}
		return nil
	})
	if err != nil && !errors.Is(err, cursorjsonl.ErrStopStreaming) {
		yield(emptyMessage(), err)
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

// sourceTimeResolver loads source timestamps and applies them to transcript messages.
type sourceTimeResolver struct {
	discovered discoveredArtifact
	opts       conversation.LoadOptions
	loaded     bool
	available  bool
	source     conversation.SourceMessages
	texts      *transcript.SourceTimeIndex
	// nextTool identifies the next native tool call to compare.
	nextTool int
	// toolsBroken prevents further tool matching after a sequence mismatch.
	toolsBroken bool
}

func newSourceTimeResolver(discovered discoveredArtifact, opts conversation.LoadOptions) *sourceTimeResolver {
	return &sourceTimeResolver{discovered: discovered, opts: opts, loaded: false, available: false, source: emptySourceMessages(), texts: nil, nextTool: 0, toolsBroken: false}
}

// load reads source messages once and reports whether they are available.
func (resolver *sourceTimeResolver) load() bool {
	if resolver.loaded {
		return resolver.available
	}
	resolver.loaded = true
	if resolver.discovered.ConversationID == "" {
		return false
	}
	ctx := context.Background()
	source, found, err := readComposerSourceMessages(ctx, resolver.discovered.ConversationID, "", resolver.opts)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_failed", "concern", concern, "path", resolver.discovered.Path, "composer_id", resolver.discovered.ConversationID, "err", err)
		return false
	}
	if !found {
		return false
	}
	resolver.source = source
	resolver.texts = newSourceTimeIndex(source)
	resolver.available = true
	return true
}

// apply adds source timestamps to a message and its tool calls when available.
func (resolver *sourceTimeResolver) apply(message *transcript.Message, body string) {
	if !message.Timestamp.IsZero() && len(message.Tools) == 0 {
		return
	}
	if !resolver.load() {
		return
	}
	if message.Timestamp.IsZero() && strings.TrimSpace(body) != "" {
		if stamp, outcome := resolver.texts.TextTime(message.Role, body); outcome == transcript.SourceTimeFound {
			message.Timestamp = stamp
		}
	}
	resolver.applyTools(message)
	if message.Timestamp.IsZero() && len(message.Tools) > 0 && strings.TrimSpace(body) == "" {
		if stamp, shared := sharedToolTime(message.Tools); shared {
			message.Timestamp = stamp
		}
	}
}

// applyTools matches tool calls by position, name, and display until a mismatch.
func (resolver *sourceTimeResolver) applyTools(message *transcript.Message) {
	for toolIndex := range message.Tools {
		if resolver.toolsBroken || !resolver.source.ToolCallsComplete {
			return
		}
		tool := &message.Tools[toolIndex]
		if resolver.nextTool >= len(resolver.source.ToolCalls) {
			resolver.breakTools("count")
			return
		}
		native := resolver.source.ToolCalls[resolver.nextTool]
		if toolIndex == 0 && !native.MessageStart {
			resolver.breakTools("boundary")
			return
		}
		if tool.Name != native.Tool.Name || !sameToolInput(tool.Input.Raw, native.Tool.Input.Raw) {
			resolver.breakTools("sequence")
			return
		}
		resolver.nextTool++
		if native.Timestamp.IsZero() {
			continue
		}
		stamp := native.Timestamp
		tool.Timestamp = &stamp
		tool.TimestampUnknown = false
	}
}

// sameToolInput compares two tool inputs as canonical JSON.
func sameToolInput(left json.RawMessage, right json.RawMessage) bool {
	leftCanonical, leftOK := canonicalToolInput(left)
	rightCanonical, rightOK := canonicalToolInput(right)
	return leftOK && rightOK && leftCanonical == rightCanonical
}

// canonicalToolInput decodes a JSON input and returns its canonical encoding.
func canonicalToolInput(raw json.RawMessage) (string, bool) {
	var builder bytes.Buffer
	if !writeCanonicalJSON(&builder, bytes.TrimSpace(raw)) {
		return "", false
	}
	return builder.String(), true
}

func writeCanonicalObject(builder *bytes.Buffer, raw []byte) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	builder.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			builder.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return false
		}
		builder.Write(encodedKey)
		builder.WriteByte(':')
		if !writeCanonicalJSON(builder, bytes.TrimSpace(object[key])) {
			return false
		}
	}
	builder.WriteByte('}')
	return true
}

// writeCanonicalJSON writes JSON with sorted object keys and compact values.
// It preserves array order and returns false when the input is invalid.
func writeCanonicalJSON(builder *bytes.Buffer, raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '{':
		return writeCanonicalObject(builder, raw)
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return false
		}
		builder.WriteByte('[')
		for index, item := range items {
			if index > 0 {
				builder.WriteByte(',')
			}
			if !writeCanonicalJSON(builder, bytes.TrimSpace(item)) {
				return false
			}
		}
		builder.WriteByte(']')
	default:
		if err := json.Compact(builder, raw); err != nil {
			return false
		}
	}
	return true
}

func (resolver *sourceTimeResolver) breakTools(reason string) {
	resolver.toolsBroken = true
	slog.Warn("providers.cursor.parser.native_tool_sequence_mismatch", "concern", concern, "path", resolver.discovered.Path, "position", resolver.nextTool, "reason", reason)
}

// sharedToolTime returns the timestamp when every tool has the same nonzero time.
func sharedToolTime(tools []transcript.ToolCall) (time.Time, bool) {
	var shared time.Time
	for index, tool := range tools {
		if tool.Timestamp == nil || tool.Timestamp.IsZero() {
			return time.Time{}, false
		}
		if index > 0 && !tool.Timestamp.Equal(shared) {
			return time.Time{}, false
		}
		shared = *tool.Timestamp
	}
	return shared, true
}

// newSourceTimeIndex indexes ordered source messages and stored copies.
func newSourceTimeIndex(source conversation.SourceMessages) *transcript.SourceTimeIndex {
	return transcript.NewSourceTimeIndex(sourceTimeEntries(source.Messages), sourceTimeEntries(source.Copies))
}

func sourceTimeEntries(messages []transcript.Message) []transcript.SourceTimeEntry {
	entries := make([]transcript.SourceTimeEntry, 0, len(messages))
	for _, message := range messages {
		entries = append(entries, transcript.SourceTimeEntry{Role: message.Role, Text: message.Text, Timestamp: message.Timestamp})
	}
	return entries
}

// readComposerSourceMessages reads a composer from the data root matching
// rootHash when rootHash is not empty. When rootHash is empty, it reads the
// composer from every configured data root that contains it. It returns an error
// when two roots return different messages or times. It reports found as false
// when no root contains the composer.
func readComposerSourceMessages(ctx context.Context, composerID string, rootHash string, opts conversation.LoadOptions) (conversation.SourceMessages, bool, error) {
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_roots_failed", "concern", concern, "composer_id", composerID, "err", err)
		return emptySourceMessages(), false, fmt.Errorf("resolve Cursor data roots: %w", err)
	}
	chosen := emptySourceMessages()
	chosenPath := ""
	for _, root := range roots {
		if rootHash != "" && RootHash(root.RootDir) != rootHash {
			continue
		}
		source, found, readErr := rootComposerSourceMessages(ctx, root.GlobalDBPath, composerID, opts)
		if readErr != nil {
			return emptySourceMessages(), false, readErr
		}
		if !found {
			continue
		}
		if chosenPath == "" {
			chosen = source
			chosenPath = root.GlobalDBPath
			continue
		}
		if sourceSignature(source) != sourceSignature(chosen) {
			conflictErr := fmt.Errorf("cursor composer %q has different messages or times in global databases %s and %s", composerID, chosenPath, root.GlobalDBPath)
			slog.WarnContext(ctx, "providers.cursor.parser.source_times_roots_conflict", "concern", concern, "composer_id", composerID, "first", chosenPath, "second", root.GlobalDBPath, "err", conflictErr)
			return emptySourceMessages(), false, conflictErr
		}
	}
	return chosen, chosenPath != "", nil
}

// rootComposerSourceMessages reads a composer's messages from the global
// database at path. It reports found as false when the database or composer
// is absent.
func rootComposerSourceMessages(ctx context.Context, path string, composerID string, opts conversation.LoadOptions) (conversation.SourceMessages, bool, error) {
	db, availability := openOptionalDatabase(ctx, path, sourceTimesComponent)
	switch availability {
	case databaseAvailabilityAbsent:
		return emptySourceMessages(), false, nil
	case databaseAvailabilityUnreadable:
		return emptySourceMessages(), false, fmt.Errorf("open Cursor global database %s", path)
	case databaseAvailabilityOpen:
	}
	source, found, readErr := composerSourceMessages(ctx, db, composerID, opts)
	closeErr := db.Close()
	if readErr != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_read_failed", "concern", concern, "path", path, "composer_id", composerID, "err", readErr)
		return emptySourceMessages(), false, fmt.Errorf("read composer from Cursor global database %s: %w", path, readErr)
	}
	if closeErr != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_close_failed", "concern", concern, "path", path, "err", closeErr)
		return emptySourceMessages(), false, fmt.Errorf("close Cursor global database %s: %w", path, closeErr)
	}
	return source, found, nil
}

// sourceSignature serializes source content and timestamps for root comparison.
func sourceSignature(source conversation.SourceMessages) string {
	var builder strings.Builder
	writeSignatureMessages(&builder, source.Messages)
	builder.WriteString(sourceCopiesMarker + "\n")
	writeSignatureMessages(&builder, source.Copies)
	fmt.Fprintf(&builder, "%s %t\n", sourceToolsMarker, source.ToolCallsComplete)
	for _, call := range source.ToolCalls {
		fmt.Fprintf(&builder, "%s%s%s%s%s%s%s%s%t\n", call.Tool.Name, sourceSignatureSeparator, call.Tool.Display, sourceSignatureSeparator, call.Timestamp.UTC().Format(time.RFC3339Nano), sourceSignatureSeparator, call.Reason, sourceSignatureSeparator, call.MessageStart)
	}
	return builder.String()
}

func writeSignatureMessages(builder *strings.Builder, messages []transcript.Message) {
	for _, message := range messages {
		builder.WriteString(message.Role + sourceSignatureSeparator + message.Text + sourceSignatureSeparator + message.Thinking + sourceSignatureSeparator + message.Timestamp.UTC().Format(time.RFC3339Nano))
		for _, tool := range message.Tools {
			builder.WriteString(sourceSignatureSeparator + tool.Name + sourceSignatureSeparator + strings.TrimSpace(tool.Display))
		}
		builder.WriteString("\n")
	}
}

func composerSourceMessages(ctx context.Context, db *sql.DB, composerID string, opts conversation.LoadOptions) (conversation.SourceMessages, bool, error) {
	source := emptySourceMessages()
	header, found, err := cursorstore.ReadComposerHeader(ctx, db, composerID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_header_failed", "concern", concern, "composer_id", composerID, "err", err)
		return source, false, fmt.Errorf("read composer header %q: %w", composerID, err)
	}
	if !found {
		return source, false, nil
	}
	kept := make(map[string]bool)
	source.Messages = make([]transcript.Message, 0)
	err = cursorstore.StreamComposerBubbles(ctx, db, composerID, header, func(bubble cursorstore.Bubble) bool {
		kept[bubble.BubbleID] = true
		mapped, include := mapComposerBubble(bubble, opts)
		if include {
			source.Messages = append(source.Messages, mapped)
		}
		return true
	})
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.source_times_bubbles_failed", "concern", concern, "composer_id", composerID, "err", err)
		return source, false, fmt.Errorf("read composer bubbles %q: %w", composerID, err)
	}
	callDates := make(map[string][]time.Time)
	source.Copies = make([]transcript.Message, 0)
	err = cursorstore.ForEachStoredComposerBubble(ctx, db, composerID, func(bubble cursorstore.Bubble) error {
		if bubble.ToolCall != nil && bubble.ToolCall.ToolCallID != "" {
			callDates[bubble.ToolCall.ToolCallID] = append(callDates[bubble.ToolCall.ToolCallID], composerBubbleTimestamp(bubble.CreatedAt))
		}
		if kept[bubble.BubbleID] {
			return nil
		}
		if mapped, include := mapComposerBubble(bubble, opts); include {
			source.Copies = append(source.Copies, mapped)
		}
		return nil
	})
	if err != nil {
		return source, false, fmt.Errorf("read stored composer bubbles %q: %w", composerID, err)
	}
	native, nativeFound, err := cursorstore.ReadComposerNativeToolCalls(ctx, db, composerID)
	if err != nil {
		return source, false, fmt.Errorf("read native tool calls for composer %q: %w", composerID, err)
	}
	source.ToolCallsComplete = nativeFound && native.Complete
	sequence := native.Sequence()
	source.ToolCalls = make([]conversation.SourceToolCall, 0, len(sequence))
	for _, entry := range sequence {
		call := nativeSourceToolCall(entry.Call, callDates[entry.Call.ID])
		call.MessageStart = entry.MessageStart
		source.ToolCalls = append(source.ToolCalls, call)
	}
	return source, true, nil
}

// nativeSourceToolCall builds a source tool call and selects its identity bubble time.
func nativeSourceToolCall(call cursorstore.NativeToolCall, dates []time.Time) conversation.SourceToolCall {
	input := transcript.ToolInputJSON{Raw: cloneRaw(call.Args)}
	display, displayLang := toolDisplayText(call.Name, input)
	tool := transcript.ToolCall{
		ID:               call.ID,
		Name:             call.Name,
		Input:            input,
		Display:          display,
		DisplayLang:      displayLang,
		Output:           "",
		IsError:          false,
		Attachments:      nil,
		Timestamp:        nil,
		TimestampUnknown: false,
	}
	result := conversation.SourceToolCall{Tool: tool, Timestamp: time.Time{}, Reason: "", MessageStart: false}
	if len(dates) == 0 || dates[0].IsZero() {
		result.Reason = SourceToolReasonIdentityAbsent
		return result
	}
	for _, stamp := range dates[1:] {
		if !stamp.Equal(dates[0]) {
			result.Reason = SourceToolReasonIdentityConflict
			return result
		}
	}
	result.Timestamp = dates[0]
	return result
}
