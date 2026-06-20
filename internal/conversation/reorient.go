package conversation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"goodkind.io/clyde/internal/conversation/memorydocs"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

const (
	// DefaultReorientWindow is the default before/after message count for the
	// rendered context windows.
	DefaultReorientWindow = 5
	// DefaultReorientSearchLimit bounds memory and fallback-search evidence items.
	DefaultReorientSearchLimit = 5
	// DefaultReorientPageBytes is the default per-page byte budget. It stays well
	// under typical harness inline thresholds so a page is read in context rather
	// than spilled to a file and grepped.
	DefaultReorientPageBytes = 7000
	// maxReorientItemRunes bounds one evidence item so a single item never
	// overflows a page on its own; longer bodies are chunked into several items.
	maxReorientItemRunes = 2400
	// reorientItemOverhead approximates the rendered framing added per item so the
	// pager budgets a little conservatively.
	reorientItemOverhead = 24
	// reorientUncompactedTail is how many trailing messages reorient shows when the
	// conversation has no compaction checkpoint.
	reorientUncompactedTail = 12
)

// ReorientItemKind names the role of one evidence item in a reorient report.
type ReorientItemKind string

const (
	// ReorientItemKindHeader summarizes the resolution: current, parent, checkpoint.
	ReorientItemKindHeader ReorientItemKind = "header"
	// ReorientItemKindRecoveredContext holds the checkpoint's compacted summary text.
	ReorientItemKindRecoveredContext ReorientItemKind = "recovered_context"
	// ReorientItemKindTail holds the in-flight work since the last compaction.
	ReorientItemKindTail ReorientItemKind = "tail"
	// ReorientItemKindParentAnchor holds the window around the fork point in the parent.
	ReorientItemKindParentAnchor ReorientItemKind = "parent_anchor"
	// ReorientItemKindMemoryDoc holds one recovered memory note.
	ReorientItemKindMemoryDoc ReorientItemKind = "memory_doc"
	// ReorientItemKindSearchHit holds one workspace-search fallback match.
	ReorientItemKindSearchHit ReorientItemKind = "search_hit"
)

// ReorientConversationRef identifies one conversation in a reorient report.
type ReorientConversationRef struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspace_root"`
}

// ReorientItem is one self-contained unit of recovered evidence. The item is the
// unit of paging: each item is bounded so it fits within one page.
type ReorientItem struct {
	Kind           ReorientItemKind `json:"kind"`
	Title          string           `json:"title"`
	Body           string           `json:"body"`
	ConversationID string           `json:"conversation_id,omitempty"`
	MessageIndex   int              `json:"message_index"`
}

// ReorientReport is the full deterministic result for one reorient resolution.
// Items is the complete ordered evidence list; the paged surface walks it with a
// cursor. The report is rebuilt deterministically on each paged call, so the
// daemon holds no per-cursor state.
type ReorientReport struct {
	CurrentConversation ReorientConversationRef  `json:"current_conversation"`
	ParentConversation  *ReorientConversationRef `json:"parent_conversation,omitempty"`
	CheckpointNumber    int                      `json:"checkpoint_number,omitempty"`
	Checkpoint          *CompactionCheckpoint    `json:"checkpoint,omitempty"`
	Items               []ReorientItem           `json:"items"`
	Warnings            []string                 `json:"warnings,omitempty"`
}

// ReorientPage is one bounded page of a reorient report plus its continuation
// cursor. Remaining is the count of items not yet delivered; while it is above
// zero the caller has not seen all evidence.
type ReorientPage struct {
	CurrentConversation ReorientConversationRef  `json:"current_conversation"`
	ParentConversation  *ReorientConversationRef `json:"parent_conversation,omitempty"`
	CheckpointNumber    int                      `json:"checkpoint_number,omitempty"`
	Items               []ReorientItem           `json:"items"`
	NextCursor          string                   `json:"next_cursor,omitempty"`
	Remaining           int                      `json:"remaining"`
	Warnings            []string                 `json:"warnings,omitempty"`
}

// ReorientOptions configures one reorient resolution.
type ReorientOptions struct {
	// ConversationID selects the current conversation. When empty, reorient picks
	// the newest conversation in WorkspaceRoot.
	ConversationID string
	// WorkspaceRoot scopes implicit current-conversation selection, memory
	// resolution, and the fallback search.
	WorkspaceRoot string
	// Topic narrows memory docs and the fallback search.
	Topic string
	// Before and After size the rendered context windows.
	Before int
	After  int
	// Limit bounds memory and fallback-search evidence items.
	Limit int
}

func normalizeReorientOptions(options ReorientOptions) ReorientOptions {
	options.ConversationID = strings.TrimSpace(options.ConversationID)
	options.WorkspaceRoot = cleanWorkspaceFilter(options.WorkspaceRoot)
	options.Topic = strings.TrimSpace(options.Topic)
	if options.Before <= 0 {
		options.Before = DefaultReorientWindow
	}
	if options.After <= 0 {
		options.After = DefaultReorientWindow
	}
	if options.Limit <= 0 {
		options.Limit = DefaultReorientSearchLimit
	}
	return options
}

// Reorient resolves the post-fork, post-compaction recovery context for a
// conversation and returns the full ordered evidence report. It reads artifacts
// only; it never mutates provider files.
func (idx *Index) Reorient(ctx context.Context, options ReorientOptions) (ReorientReport, error) {
	options = normalizeReorientOptions(options)
	current, err := idx.resolveReorientCurrent(ctx, options)
	if err != nil {
		return ReorientReport{}, err
	}

	report := ReorientReport{
		CurrentConversation: conversationRef(current),
		ParentConversation:  nil,
		CheckpointNumber:    0,
		Checkpoint:          nil,
		Items:               nil,
		Warnings:            nil,
	}

	messages, err := idx.LoadMessagesWithOptions(current, LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	})
	if err != nil {
		return ReorientReport{}, err
	}

	checkpointNumber, checkpoint := LatestReorientCheckpoint(CompactionCheckpoints(messages))
	report.CheckpointNumber = checkpointNumber
	report.Checkpoint = checkpoint

	parent, hasParent, err := ResolveForkParent(ctx, idx, current)
	if err != nil {
		return ReorientReport{}, err
	}
	if hasParent {
		parentRef := conversationRef(parent)
		report.ParentConversation = &parentRef
	} else if current.Lineage != nil && current.Lineage.Kind == ConversationLineageKindFork {
		report.Warnings = append(report.Warnings, "fork parent not in index; falling back to memory and search")
	}

	// Order the evidence so the orienting frame comes before the bulky tail: the
	// recovered summary and the parent fork anchor first, then the in-flight work
	// since the last compaction, then memory and search.
	var items []ReorientItem
	items = appendRecoveredContextItems(items, current, checkpoint)
	items = idx.appendParentAnchorItem(items, current, parent, hasParent, options, &report)
	items = appendTailItems(items, current, checkpoint, messages)
	items = idx.appendMemoryItems(items, current, parent, hasParent, options)
	if !hasParent {
		items = idx.appendSearchItems(ctx, items, current, options)
	}

	header := buildReorientHeader(current, report.ParentConversation, report.Warnings, checkpointNumber, checkpoint)
	report.Items = append([]ReorientItem{header}, items...)
	return report, nil
}

// ReorientPageText resolves a reorient report and returns one paged slice of it.
// asJSON selects the typed ReorientPage JSON document; otherwise it returns the
// brief-input text surface. nextCursor is empty on the final page, and remaining
// is the count of undelivered items.
func (idx *Index) ReorientPageText(ctx context.Context, options ReorientOptions, cursor string, pageBytes int, asJSON bool) (text string, nextCursor string, remaining int, err error) {
	report, err := idx.Reorient(ctx, options)
	if err != nil {
		return "", "", 0, err
	}
	offset, err := unmarshalReorientCursor(cursor)
	if err != nil {
		return "", "", 0, err
	}
	if pageBytes <= 0 {
		pageBytes = DefaultReorientPageBytes
	}
	pageItems, nextOffset := pageReorientItems(report.Items, offset, pageBytes)
	remaining = len(report.Items) - nextOffset
	if nextOffset < len(report.Items) {
		nextCursor = encodeReorientCursor(nextOffset)
	}
	if asJSON {
		text, err = marshalReorientPageJSON(report, pageItems, nextCursor, remaining)
		return text, nextCursor, remaining, err
	}
	text = renderReorientPageText(pageItems, offset, len(report.Items), remaining, nextCursor)
	return text, nextCursor, remaining, nil
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

// appendRecoveredContextItems appends the checkpoint's recovered summary text.
// It adds nothing when the conversation is uncompacted or the checkpoint carries
// no summary-class text, which is common for Codex boundaries.
func appendRecoveredContextItems(items []ReorientItem, current Record, checkpoint *CompactionCheckpoint) []ReorientItem {
	if checkpoint == nil {
		return items
	}
	summary := renderCheckpointSummary(*checkpoint)
	return appendChunkedItems(items, ReorientItemKindRecoveredContext, "Recovered context (checkpoint summary)", current.ID, checkpoint.SummaryIndex, summary)
}

// appendTailItems appends the in-flight work. For a compacted conversation that
// is everything after the latest boundary; for an uncompacted one it is the last
// few messages. The tail is the bulkiest evidence, so it follows the orienting
// items.
func appendTailItems(items []ReorientItem, current Record, checkpoint *CompactionCheckpoint, messages []transcript.Message) []ReorientItem {
	if checkpoint == nil {
		start := max(len(messages)-reorientUncompactedTail, 0)
		body := transcript.RenderPlainTextWithOptions(messages[start:], transcript.DefaultShapeOptions())
		return appendChunkedItems(items, ReorientItemKindTail, "Recent messages (uncompacted conversation)", current.ID, start, body)
	}
	tailStart := checkpointTailStart(*checkpoint)
	if tailStart >= len(messages) {
		return items
	}
	body := transcript.RenderPlainTextWithOptions(messages[tailStart:], transcript.DefaultShapeOptions())
	return appendChunkedItems(items, ReorientItemKindTail, "In-flight work since last compaction", current.ID, tailStart, body)
}

func (idx *Index) appendParentAnchorItem(items []ReorientItem, current Record, parent Record, hasParent bool, options ReorientOptions, report *ReorientReport) []ReorientItem {
	if !hasParent || current.Lineage == nil || current.Lineage.ParentMessageUUID == "" {
		return items
	}
	body, index, ok, err := idx.parentAnchorWindow(parent, current.Lineage.ParentMessageUUID, options.Before, options.After)
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("parent anchor window: %v", err))
		return items
	}
	if !ok {
		report.Warnings = append(report.Warnings, "parent anchor message not found in parent transcript")
		return items
	}
	return append(items, ReorientItem{
		Kind:           ReorientItemKindParentAnchor,
		Title:          "Parent fork anchor",
		Body:           body,
		ConversationID: parent.ID,
		MessageIndex:   index,
	})
}

func (idx *Index) parentAnchorWindow(parent Record, parentMessageUUID string, before int, after int) (string, int, bool, error) {
	messages, err := idx.LoadMessagesWithOptions(parent, LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	})
	if err != nil {
		return "", 0, false, err
	}
	for index, message := range messages {
		if message.UUID == parentMessageUUID {
			return renderContextWindow(messages, len(messages), index, before, after), index, true, nil
		}
	}
	return "", 0, false, nil
}

func (idx *Index) appendMemoryItems(items []ReorientItem, current Record, parent Record, hasParent bool, options ReorientOptions) []ReorientItem {
	docs := loadReorientMemory(current, parent, hasParent)
	terms := queryTerms(options.Topic)
	nonIndexAdded := 0
	for _, doc := range docs {
		title := fmt.Sprintf("Memory (%s): %s", doc.Provider, doc.Title)
		if doc.Index {
			items = appendChunkedItems(items, ReorientItemKindMemoryDoc, title, "", -1, doc.Body)
			continue
		}
		if len(terms) == 0 || !docMatchesTerms(doc, terms) {
			continue
		}
		if nonIndexAdded >= options.Limit {
			continue
		}
		items = appendChunkedItems(items, ReorientItemKindMemoryDoc, title, "", -1, doc.Body)
		nonIndexAdded++
	}
	return items
}

func loadReorientMemory(current Record, parent Record, hasParent bool) []memorydocs.Doc {
	docs, err := memorydocs.Load(current.Provider, current.WorkspaceRoot)
	if err != nil {
		docs = nil
	}
	if !hasParent || parent.Provider == current.Provider {
		return docs
	}
	parentDocs, err := memorydocs.Load(parent.Provider, parent.WorkspaceRoot)
	if err != nil {
		return docs
	}
	seen := make(map[string]struct{}, len(docs))
	for _, doc := range docs {
		seen[doc.Path] = struct{}{}
	}
	for _, doc := range parentDocs {
		if _, ok := seen[doc.Path]; ok {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}

func docMatchesTerms(doc memorydocs.Doc, terms []string) bool {
	haystack := strings.ToLower(doc.Title + "\n" + doc.Body)
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func (idx *Index) appendSearchItems(ctx context.Context, items []ReorientItem, current Record, options ReorientOptions) []ReorientItem {
	if options.Topic == "" {
		return items
	}
	result, err := idx.SearchConversations(ctx, SearchConversationsOptions{
		Query:                options.Topic,
		Limit:                options.Limit,
		Provider:             providerid.ProviderUnspecified,
		WorkspaceRoot:        current.WorkspaceRoot,
		IncludeArchived:      false,
		Roles:                nil,
		FromUnix:             0,
		UntilUnix:            0,
		MinScore:             0,
		PerConversationLimit: 0,
		ConversationID:       "",
		ContextWindow:        0,
	})
	if err != nil {
		return items
	}
	for _, match := range result.Matches {
		if match.Record.ID == current.ID {
			continue
		}
		body := fmt.Sprintf(
			"%s\n\nDeeper: clyde conversation search --conversation %s --around %d --window %d",
			match.Snippet,
			match.Record.ID,
			match.MessageIndex,
			options.After,
		)
		items = append(items, ReorientItem{
			Kind:           ReorientItemKindSearchHit,
			Title:          "Search match: " + reorientTitle(match.Record),
			Body:           body,
			ConversationID: match.Record.ID,
			MessageIndex:   match.MessageIndex,
		})
	}
	return items
}

func buildReorientHeader(current Record, parent *ReorientConversationRef, warnings []string, checkpointNumber int, checkpoint *CompactionCheckpoint) ReorientItem {
	var builder strings.Builder
	fmt.Fprintf(&builder, "current: %s", current.ID)
	if current.Title != "" {
		fmt.Fprintf(&builder, " (%s)", current.Title)
	}
	builder.WriteString("\n")
	if parent != nil {
		kind := "fork"
		if current.Lineage != nil {
			kind = string(current.Lineage.Kind)
		}
		fmt.Fprintf(&builder, "parent:  %s [%s]\n", parent.ID, kind)
	} else {
		builder.WriteString("parent:  none (no fork lineage; same-conversation recovery)\n")
	}
	if checkpoint != nil {
		fmt.Fprintf(
			&builder,
			"checkpoint: %d  boundary=%d summary=%d messages_summarized=%d usable=%t\n",
			checkpointNumber,
			checkpoint.BoundaryIndex,
			checkpoint.SummaryIndex,
			checkpoint.MessagesSummarized,
			checkpoint.HasUsableCompactedContext(),
		)
	} else {
		builder.WriteString("checkpoint: none (uncompacted; current context is the whole conversation)\n")
	}
	for _, warning := range warnings {
		fmt.Fprintf(&builder, "warning: %s\n", warning)
	}
	return ReorientItem{
		Kind:           ReorientItemKindHeader,
		Title:          "Reorient",
		Body:           strings.TrimRight(builder.String(), "\n"),
		ConversationID: current.ID,
		MessageIndex:   -1,
	}
}

func renderCheckpointSummary(checkpoint CompactionCheckpoint) string {
	var builder strings.Builder
	for _, item := range checkpoint.ContextItems {
		if item.Kind != transcript.CompactedContextItemKindMessage || item.Message == nil {
			continue
		}
		if item.Message.MessageClass != transcript.CompactedMessageClassSummary {
			continue
		}
		for _, content := range item.Message.Content {
			text := strings.TrimSpace(content.Text)
			if text == "" {
				continue
			}
			builder.WriteString(text)
			builder.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(builder.String())
}

func conversationRef(record Record) ReorientConversationRef {
	return ReorientConversationRef{
		ID:            record.ID,
		Provider:      record.Provider.String(),
		Title:         record.Title,
		WorkspaceRoot: record.WorkspaceRoot,
	}
}

func reorientTitle(record Record) string {
	if record.Title != "" {
		return record.Title
	}
	return record.ID
}

// appendChunkedItems appends body as one or more items, splitting on line
// boundaries so no single item exceeds maxReorientItemRunes. An empty body adds
// nothing.
func appendChunkedItems(items []ReorientItem, kind ReorientItemKind, title string, conversationID string, messageIndex int, body string) []ReorientItem {
	body = strings.TrimSpace(body)
	if body == "" {
		return items
	}
	chunks := chunkText(body, maxReorientItemRunes)
	for index, chunk := range chunks {
		itemTitle := title
		if len(chunks) > 1 {
			itemTitle = fmt.Sprintf("%s (part %d/%d)", title, index+1, len(chunks))
		}
		items = append(items, ReorientItem{
			Kind:           kind,
			Title:          itemTitle,
			Body:           chunk,
			ConversationID: conversationID,
			MessageIndex:   messageIndex,
		})
	}
	return items
}

// chunkText splits text into chunks of at most maxRunes runes, breaking on line
// boundaries where possible. A single line longer than maxRunes is hard-split.
func chunkText(text string, maxRunes int) []string {
	if maxRunes <= 0 || utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	var builder strings.Builder
	count := 0
	flush := func() {
		if builder.Len() > 0 {
			chunks = append(chunks, strings.TrimRight(builder.String(), "\n"))
			builder.Reset()
			count = 0
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		lineRunes := utf8.RuneCountInString(line) + 1
		if count > 0 && count+lineRunes > maxRunes {
			flush()
		}
		if lineRunes > maxRunes {
			flush()
			chunks = append(chunks, splitByRunes(line, maxRunes)...)
			continue
		}
		builder.WriteString(line)
		builder.WriteString("\n")
		count += lineRunes
	}
	flush()
	if len(chunks) == 0 {
		return []string{text}
	}
	return chunks
}

func splitByRunes(text string, maxRunes int) []string {
	runes := []rune(text)
	var pieces []string
	for start := 0; start < len(runes); start += maxRunes {
		end := min(start+maxRunes, len(runes))
		pieces = append(pieces, string(runes[start:end]))
	}
	return pieces
}

// pageReorientItems returns the items starting at offset that fit within
// pageBytes, plus the next offset. It always returns at least one item so paging
// makes progress even when a single item exceeds the budget.
func pageReorientItems(items []ReorientItem, offset int, pageBytes int) ([]ReorientItem, int) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return nil, len(items)
	}
	size := 0
	end := offset
	for end < len(items) {
		itemSize := reorientItemBytes(items[end])
		if end > offset && size+itemSize > pageBytes {
			break
		}
		size += itemSize
		end++
	}
	if end == offset {
		end = offset + 1
	}
	return items[offset:end], end
}

func reorientItemBytes(item ReorientItem) int {
	return len(item.Title) + len(item.Body) + reorientItemOverhead
}

type reorientCursor struct {
	Offset int `json:"offset"`
}

func encodeReorientCursor(offset int) string {
	payload, err := json.Marshal(reorientCursor{Offset: offset})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func unmarshalReorientCursor(cursor string) (int, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		slog.Warn("conversation.reorient.cursor_decode_failed", "concern", "conversation.reorient", "component", "conversation", "err", err)
		return 0, fmt.Errorf("decode reorient cursor: %w", err)
	}
	var decoded reorientCursor
	if err := json.Unmarshal(payload, &decoded); err != nil {
		slog.Warn("conversation.reorient.cursor_decode_failed", "concern", "conversation.reorient", "component", "conversation", "err", err)
		return 0, fmt.Errorf("decode reorient cursor: %w", err)
	}
	if decoded.Offset < 0 {
		return 0, fmt.Errorf("reorient cursor offset is negative")
	}
	return decoded.Offset, nil
}

func renderReorientPageText(pageItems []ReorientItem, offset int, total int, remaining int, nextCursor string) string {
	var builder strings.Builder
	for _, item := range pageItems {
		fmt.Fprintf(&builder, "## %s\n\n%s\n\n", item.Title, item.Body)
	}
	last := offset + len(pageItems)
	fmt.Fprintf(&builder, "---\nitems %d-%d of %d; remaining %d\n", offset+1, last, total, remaining)
	if remaining > 0 {
		fmt.Fprintf(&builder, "Not finished: call reorient again with cursor=%s. Read every page before reasoning.\n", nextCursor)
	} else {
		builder.WriteString("All evidence delivered. You may now write the reorientation brief.\n")
	}
	return builder.String()
}

func marshalReorientPageJSON(report ReorientReport, pageItems []ReorientItem, nextCursor string, remaining int) (string, error) {
	page := ReorientPage{
		CurrentConversation: report.CurrentConversation,
		ParentConversation:  report.ParentConversation,
		CheckpointNumber:    report.CheckpointNumber,
		Items:               pageItems,
		NextCursor:          nextCursor,
		Remaining:           remaining,
		Warnings:            report.Warnings,
	}
	body, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		slog.Warn("conversation.reorient.page_marshal_failed", "concern", "conversation.reorient", "component", "conversation", "err", err)
		return "", fmt.Errorf("render reorient page json: %w", err)
	}
	return string(body), nil
}
