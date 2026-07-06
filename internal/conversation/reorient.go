package conversation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode/utf8"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

const (
	// DefaultReorientMaxLines caps the recovered transcript to its last N lines
	// so one reorient stays bounded regardless of how large the pre-compaction
	// history was.
	DefaultReorientMaxLines = 3500
	// DefaultReorientPageBytes is the default per-page byte budget. It stays well
	// under typical harness inline thresholds so a page is read in context rather
	// than spilled to a file and grepped.
	DefaultReorientPageBytes = 30000
)

// ReorientConversationRef identifies one conversation in a reorient report.
type ReorientConversationRef struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspace_root"`
}

// ReorientPage is one bounded page of the recovered pre-compaction transcript
// plus its continuation cursor. The recovered transcript is a fixed snapshot of
// the conversation before the latest compaction point, so paging over it is
// stable: the same cursor always returns the same bytes while the live thread
// keeps growing after the compaction point.
type ReorientPage struct {
	CurrentConversation ReorientConversationRef `json:"current_conversation"`
	// Body is this page's slice of the recovered transcript.
	Body string `json:"body"`
	// NextCursor continues to the next page. Empty on the final page.
	NextCursor string `json:"next_cursor,omitempty"`
	// Remaining is the count of recovered bytes not yet delivered.
	Remaining int `json:"remaining"`
	// Offset is the byte offset of Body within the whole recovered transcript.
	Offset int `json:"offset"`
	// TotalBytes is the size of the whole recovered transcript.
	TotalBytes int `json:"total_bytes"`
	// TotalLines is the line count of the whole recovered transcript.
	TotalLines int `json:"total_lines"`
	// Truncated reports that the line cap dropped older lines from the top.
	Truncated bool `json:"truncated,omitempty"`
	// Restart reports that the conversation was compacted again since the cursor
	// was issued, so paging restarted from the first page of the new transcript.
	Restart  bool     `json:"restart,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ReorientOptions configures one reorient resolution.
type ReorientOptions struct {
	// ConversationID selects the current conversation. When empty, reorient picks
	// the newest conversation in WorkspaceRoot.
	ConversationID string
	// WorkspaceRoot scopes implicit current-conversation selection.
	WorkspaceRoot string
	// MaxLines caps the recovered transcript to its last N lines. Zero uses
	// DefaultReorientMaxLines.
	MaxLines int
	// IncludeToolOutputs renders full tool result bodies instead of the default
	// tool-call commands.
	IncludeToolOutputs bool
	// SyntheticPreCompact recovers the current conversation, for hooks that run
	// before the provider compacts and there is no boundary yet. The result is
	// still capped to MaxLines.
	SyntheticPreCompact bool
}

func normalizeReorientOptions(options ReorientOptions) ReorientOptions {
	options.ConversationID = strings.TrimSpace(options.ConversationID)
	options.WorkspaceRoot = cleanWorkspaceFilter(options.WorkspaceRoot)
	if options.MaxLines <= 0 {
		options.MaxLines = DefaultReorientMaxLines
	}
	return options
}

// reorientSnapshot is the fixed recovered transcript for one conversation plus
// the identity of the compaction point it belongs to.
type reorientSnapshot struct {
	Current     ReorientConversationRef
	Body        string
	Fingerprint string
	TotalLines  int
	Truncated   bool
	Warnings    []string
}

// buildReorientSnapshot renders the pre-compaction history as a dense chat plus
// tool-call transcript, capped to the last MaxLines lines. It reads artifacts
// only; it never mutates provider files.
func (idx *Index) buildReorientSnapshot(ctx context.Context, options ReorientOptions) (reorientSnapshot, error) {
	current, err := idx.resolveReorientCurrent(ctx, options)
	if err != nil {
		return reorientSnapshot{}, err
	}
	return idx.renderReorientSnapshot(current, options)
}

// renderReorientSnapshot renders one already-resolved Record into the recovered
// transcript snapshot. It reads the artifact only and never consults the index,
// so a caller that built the Record directly from a path renders it without the
// resolve/refresh race.
func (idx *Index) renderReorientSnapshot(current Record, options ReorientOptions) (reorientSnapshot, error) {
	content := NewContentKindSet(ContentKindChat, ContentKindToolCalls)
	if options.IncludeToolOutputs {
		content = NewContentKindSet(ContentKindChat, ContentKindToolOutputs)
	}
	// Load the same message set Export will select over, with the compaction
	// boundary records included, so the segment indexes and the latest-boundary
	// fingerprint line up with the export selection.
	messages, err := idx.LoadMessagesWithOptions(current, LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    content.Has(ContentKindToolOutputs),
	})
	if err != nil {
		return reorientSnapshot{}, err
	}
	selector := reorientSelector(messages, options.SyntheticPreCompact)
	fingerprint := reorientFingerprint(messages)
	body, err := idx.Export(current, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		LastN:        0,
		MaxLines:     0,
		Whitespace:   WhitespaceDense,
		Content:      content,
		Compaction:   CompactionExportOptions{IncludeSelector: selector, FullHistory: false},
	})
	if err != nil {
		return reorientSnapshot{}, err
	}
	capped, totalLines, truncated := capToLastLines(string(body), options.MaxLines)
	var warnings []string
	if truncated {
		warnings = append(warnings, fmt.Sprintf("recovered transcript truncated to the last %d lines; older detail was dropped", options.MaxLines))
	}
	if fingerprint == "" {
		warnings = append(warnings, "conversation has no compaction point yet; recovering the current conversation")
	}
	return reorientSnapshot{
		Current:     conversationRef(current),
		Body:        capped,
		Fingerprint: fingerprint,
		TotalLines:  totalLines,
		Truncated:   truncated,
		Warnings:    warnings,
	}, nil
}

// reorientSelector picks the compaction segments to recover. Segment 0 is the
// current tail after the latest compaction point, which the agent still holds,
// so reorient recovers segments 1..N, the history before that point. An
// uncompacted conversation has only segment 0, so it recovers that instead.
func reorientSelector(messages []transcript.Message, syntheticPreCompact bool) string {
	if syntheticPreCompact {
		return "all"
	}
	segments := CompactionSegments(messages)
	maxIndex := maxCompactionSegmentIndex(segments)
	if maxIndex < 1 {
		return defaultCompactionSelector
	}
	return fmt.Sprintf("1..%d", maxIndex)
}

// reorientFingerprint identifies the latest compaction point so a cursor issued
// against one snapshot can detect a later compaction. It is empty when the
// conversation has no compaction point yet.
func reorientFingerprint(messages []transcript.Message) string {
	checkpoints := CompactionCheckpoints(messages)
	if len(checkpoints) == 0 {
		return ""
	}
	latest := checkpoints[len(checkpoints)-1]
	if latest.SummaryUUID != "" {
		return latest.SummaryUUID
	}
	return latest.BoundaryUUID
}

// ReorientPage builds the recovered transcript snapshot and returns one bounded
// page of it. The snapshot is rebuilt on each call, but it is the pre-compaction
// history, which does not change as the live thread keeps writing after the
// compaction point, so the byte offset in the cursor addresses the same content
// on every call.
func (idx *Index) ReorientPage(ctx context.Context, options ReorientOptions, cursor string, pageBytes int) (ReorientPage, error) {
	options = normalizeReorientOptions(options)
	snapshot, err := idx.buildReorientSnapshot(ctx, options)
	if err != nil {
		return ReorientPage{}, err
	}
	if pageBytes <= 0 {
		pageBytes = DefaultReorientPageBytes
	}
	decoded, err := decodeReorientCursor(cursor)
	if err != nil {
		return ReorientPage{}, err
	}
	offset := decoded.Offset
	restart := false
	warnings := snapshot.Warnings
	if decoded.Fingerprint != "" && decoded.Fingerprint != snapshot.Fingerprint {
		restart = true
		offset = 0
		warnings = append(warnings, "conversation was compacted again since the cursor was issued; restarting reorient from the first page")
	}
	total := len(snapshot.Body)
	if offset > total {
		offset = total
	}
	pageText, nextOffset := sliceReorientBody(snapshot.Body, offset, pageBytes)
	nextCursor := ""
	if nextOffset < total {
		nextCursor = encodeReorientCursor(reorientCursor{Fingerprint: snapshot.Fingerprint, Offset: nextOffset})
	}
	page := ReorientPage{
		CurrentConversation: snapshot.Current,
		Body:                pageText,
		NextCursor:          nextCursor,
		Remaining:           total - nextOffset,
		Offset:              offset,
		TotalBytes:          total,
		TotalLines:          snapshot.TotalLines,
		Truncated:           snapshot.Truncated,
		Restart:             restart,
		Warnings:            warnings,
	}
	logReorientResolved(ctx, snapshot, page)
	return page, nil
}

// ReorientPageText builds the recovered transcript snapshot and returns one
// paged slice of it. asJSON selects the typed ReorientPage JSON document;
// otherwise it returns the brief-input text surface. nextCursor is empty on the
// final page, and remaining is the count of undelivered bytes.
func (idx *Index) ReorientPageText(ctx context.Context, options ReorientOptions, cursor string, pageBytes int, asJSON bool) (text string, nextCursor string, remaining int, err error) {
	page, err := idx.ReorientPage(ctx, options, cursor, pageBytes)
	if err != nil {
		return "", "", 0, err
	}
	if asJSON {
		text, err = marshalReorientPageJSON(page)
		return text, page.NextCursor, page.Remaining, err
	}
	return RenderReorientPageText(page), page.NextCursor, page.Remaining, nil
}

func (idx *Index) resolveReorientCurrent(ctx context.Context, options ReorientOptions) (Record, error) {
	if options.ConversationID != "" {
		return idx.Resolve(ctx, options.ConversationID)
	}
	if options.WorkspaceRoot == "" {
		return Record{}, fmt.Errorf("reorient requires conversation_id or workspace")
	}
	page, err := idx.ListPage(ctx, ListOptions{
		Limit:           1,
		Offset:          0,
		Provider:        providerid.ProviderUnspecified,
		WorkspaceRoot:   options.WorkspaceRoot,
		Query:           "",
		IncludeArchived: false,
		All:             false,
	})
	if err != nil {
		return Record{}, err
	}
	if len(page.Records) == 0 {
		return Record{}, fmt.Errorf("no conversations found for workspace %q", options.WorkspaceRoot)
	}
	return page.Records[0], nil
}

// RenderReorientArtifact recovers a conversation from a concrete on-disk
// transcript path and returns its rendered pre-compaction transcript body. It
// builds the Record directly through the provider parser rather than the index,
// so a caller that already holds the path (the MITM reorient-injection hook,
// which resolves the session id from the intercepted request to a transcript
// file) recovers the conversation without the index resolve/refresh race. The
// options select the render knobs (tool outputs, line cap, synthetic
// pre-compact recovery).
func (idx *Index) RenderReorientArtifact(path string, preferred providerid.Provider, options ReorientOptions) (string, error) {
	options = normalizeReorientOptions(options)
	record, ok, err := idx.resolveArtifactRecord(path, preferred)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("reorient artifact %q: no registered provider parsed it", path)
	}
	snapshot, err := idx.renderReorientSnapshot(record, options)
	if err != nil {
		return "", err
	}
	return snapshot.Body, nil
}

// resolveArtifactRecord builds a Record for a concrete existing transcript path
// through the provider parsers, without consulting the index. It scans the
// preferred provider first when one is given, so a caller that knows the artifact
// belongs to a specific provider (the reorient-injection hook always resolves a
// Claude transcript) does not trigger every other provider's scan-failure logs.
// It falls back to the remaining providers so a hintless caller still resolves.
// The second return is false when the path exists but no provider recognizes it.
func (idx *Index) resolveArtifactRecord(path string, preferred providerid.Provider) (Record, bool, error) {
	var zero Record
	info, err := os.Stat(path)
	if err != nil {
		slog.Warn("conversation.reorient.artifact_stat_failed", "concern", "conversation.reorient", "component", "conversation", "path", path, "err", err)
		return zero, false, fmt.Errorf("stat reorient artifact %q: %w", path, err)
	}
	if info.IsDir() {
		slog.Warn("conversation.reorient.artifact_not_file", "concern", "conversation.reorient", "component", "conversation", "path", path)
		return zero, false, fmt.Errorf("reorient artifact %q is a directory", path)
	}
	stamp := FileStamp{Size: info.Size(), Mtime: info.ModTime()}
	if preferred.Valid() {
		if parser, lookupErr := idx.registry.Lookup(preferred); lookupErr == nil {
			if record, ok := parser.ScanRecord(path, stamp); ok {
				return record, true, nil
			}
		}
	}
	for _, provider := range idx.registry.Providers() {
		if provider == preferred {
			continue
		}
		parser, lookupErr := idx.registry.Lookup(provider)
		if lookupErr != nil {
			continue
		}
		record, ok := parser.ScanRecord(path, stamp)
		if ok {
			return record, true, nil
		}
	}
	return zero, false, nil
}

// sliceReorientBody returns the slice of body starting at offset that fits
// within pageBytes, broken on a line boundary, plus the next offset. It always
// advances so paging makes progress even when a single line exceeds the budget.
func sliceReorientBody(body string, offset int, pageBytes int) (string, int) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(body) {
		return "", len(body)
	}
	if pageBytes < 1 {
		pageBytes = 1
	}
	end := offset + pageBytes
	if end >= len(body) {
		return body[offset:], len(body)
	}
	if boundary := strings.LastIndexByte(body[offset:end], '\n'); boundary >= 0 {
		end = offset + boundary + 1
		return body[offset:end], end
	}
	// No newline fits in the budget. Hard-cut, but pull the cut back to a rune
	// boundary so a page never ends mid-rune and produces invalid UTF-8.
	for end > offset && !utf8.RuneStart(body[end]) {
		end--
	}
	if end == offset {
		// A single rune wider than the budget: take the whole rune so paging
		// still advances.
		_, size := utf8.DecodeRuneInString(body[offset:])
		end = offset + size
	}
	return body[offset:end], end
}

func conversationRef(record Record) ReorientConversationRef {
	return ReorientConversationRef{
		ID:            record.ID,
		Provider:      record.Provider.String(),
		Title:         record.Title,
		WorkspaceRoot: record.WorkspaceRoot,
	}
}

type reorientCursor struct {
	Fingerprint string `json:"f"`
	Offset      int    `json:"o"`
}

func encodeReorientCursor(cursor reorientCursor) string {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeReorientCursor(cursor string) (reorientCursor, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return reorientCursor{Fingerprint: "", Offset: 0}, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		slog.Warn("conversation.reorient.cursor_decode_failed", "concern", "conversation.reorient", "component", "conversation", "err", err)
		return reorientCursor{}, fmt.Errorf("decode reorient cursor: %w", err)
	}
	var decoded reorientCursor
	if err := json.Unmarshal(payload, &decoded); err != nil {
		slog.Warn("conversation.reorient.cursor_decode_failed", "concern", "conversation.reorient", "component", "conversation", "err", err)
		return reorientCursor{}, fmt.Errorf("decode reorient cursor: %w", err)
	}
	if decoded.Offset < 0 {
		return reorientCursor{}, fmt.Errorf("reorient cursor offset is negative")
	}
	return decoded, nil
}

func logReorientResolved(ctx context.Context, snapshot reorientSnapshot, page ReorientPage) {
	slog.InfoContext(
		ctx,
		"conversation.reorient.resolved",
		"concern",
		"conversation.reorient",
		"component",
		"conversation",
		"conversation_id",
		snapshot.Current.ID,
		"fingerprint",
		snapshot.Fingerprint,
		"offset",
		page.Offset,
		"remaining",
		page.Remaining,
		"total_bytes",
		page.TotalBytes,
		"total_lines",
		page.TotalLines,
		"truncated",
		page.Truncated,
		"restart",
		page.Restart,
		"warning_count",
		len(page.Warnings),
	)
}
