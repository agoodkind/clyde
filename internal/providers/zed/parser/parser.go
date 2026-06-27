// Package parser reads local Zed thread databases and implements the earliest
// Zed parser hooks for Clyde's conversation index.
package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	zedstore "goodkind.io/clyde/internal/providers/zed/store"
	"goodkind.io/clyde/internal/transcript"
)

const (
	concern               = "providers.zed.parser"
	artifactKindZedThread = "zed_thread"
)

type discoveredThread struct {
	Row      zedstore.ThreadRow
	Metadata zedstore.SidebarThreadMetadata
	RootDir  string
	Channel  string
}

type metadataWithChannel struct {
	Metadata zedstore.SidebarThreadMetadata
	Channel  string
}

// Parser discovers Zed thread candidates and caches the rows needed by later
// scan stages.
type Parser struct {
	mu         sync.Mutex
	discovered map[string]discoveredThread
}

var _ conversation.Parser = (*Parser)(nil)

// New returns a Zed conversation parser.
func New() *Parser {
	return &Parser{mu: sync.Mutex{}, discovered: make(map[string]discoveredThread)}
}

// Provider reports that this parser handles Zed artifacts.
func (*Parser) Provider() providerid.Provider {
	return providerid.ProviderZed
}

// Discover resolves local Zed data roots, reads thread rows plus sidebar
// metadata, and returns native Zed thread candidates.
func (p *Parser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	roots, err := zedstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.resolve_roots_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("resolve zed data roots: %w", err)
	}

	candidates := make([]conversation.ScanCandidate, 0)
	discovered := make(map[string]discoveredThread)
	for _, root := range roots {
		rows, err := readThreadRowsForRoot(ctx, root)
		if err != nil {
			return nil, err
		}
		rows = parsableThreadRows(ctx, root.ThreadsDBPath, rows)
		if len(rows) == 0 {
			continue
		}

		metadataBySession, err := readMetadataForRoot(ctx, root)
		if err != nil {
			return nil, err
		}

		for _, row := range rows {
			metadata, ok := metadataBySession[row.SessionID]
			if !ok || metadata.Metadata.AgentID != "" {
				continue
			}
			path := BuildVirtualPath(root.RootDir, metadata.Channel, row.SessionID)
			if path == "" {
				continue
			}
			candidates = append(candidates, conversation.ScanCandidate{
				Path:  path,
				Stamp: conversation.FileStamp{Size: int64(len(row.Data)), Mtime: row.UpdatedAt},
			})
			discovered[path] = discoveredThread{
				Row:      row,
				Metadata: metadata.Metadata,
				RootDir:  root.RootDir,
				Channel:  metadata.Channel,
			}
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Path < candidates[j].Path
	})

	p.mu.Lock()
	p.discovered = discovered
	p.mu.Unlock()

	return candidates, nil
}

// ScanRecord turns one discovered native Zed thread into a derived Clyde
// record without streaming the full transcript.
func (p *Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	p.mu.Lock()
	discovered, ok := p.discovered[path]
	p.mu.Unlock()
	if !ok {
		return emptyRecord(), false
	}

	thread, err := zedstore.ParseThreadDocument(discovered.Row.DataType, discovered.Row.Data)
	if err != nil {
		return emptyRecord(), false
	}

	return conversation.Record{
		ID:            conversation.DerivedID(providerid.ProviderZed, discovered.Row.SessionID, path),
		Provider:      providerid.ProviderZed,
		NativeID:      discovered.Row.SessionID,
		Lineage:       buildLineage(discovered.Row.ParentSessionID, thread.SubagentContext),
		Title:         resolvedTitle(discovered.Metadata, thread),
		WorkspaceRoot: firstNonEmptyString(firstPath(discovered.Metadata.FolderPaths), firstPath(discovered.Row.FolderPaths)),
		ArtifactPath:  path,
		ArtifactKind:  artifactKindZedThread,
		Model:         resolvedModel(thread.Model),
		CreatedAt:     firstNonZeroTime(discovered.Metadata.CreatedAt, discovered.Row.CreatedAt, thread.UpdatedAt),
		UpdatedAt:     firstNonZeroTime(discovered.Metadata.UpdatedAt, discovered.Row.UpdatedAt, thread.UpdatedAt),
		SizeBytes:     stamp.Size,
		Archived:      discovered.Metadata.Archived,
	}, true
}

// Stream lazily yields transcript-shaped messages for one discovered Zed
// thread.
func (p *Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		p.mu.Lock()
		discovered, ok := p.discovered[path]
		p.mu.Unlock()
		if !ok {
			yield(emptyMessage(), fmt.Errorf("zed stream path not discovered: %s", path))
			return
		}
		thread, err := zedstore.ParseThreadDocument(discovered.Row.DataType, discovered.Row.Data)
		if err != nil {
			yield(emptyMessage(), fmt.Errorf("parse zed thread document: %w", err))
			return
		}
		for _, message := range thread.Messages {
			mapped, include := transcriptMessage(thread, message, opts)
			if !include {
				continue
			}
			if !yield(mapped, nil) {
				return
			}
		}
	}
}

func emptyRecord() conversation.Record {
	var record conversation.Record
	return record
}

func emptyMessage() transcript.Message {
	var message transcript.Message
	return message
}

func readThreadRowsForRoot(ctx context.Context, root zedstore.DataRoot) ([]zedstore.ThreadRow, error) {
	if _, err := os.Stat(root.ThreadsDBPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		slog.WarnContext(ctx, "providers.zed.parser.stat_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, fmt.Errorf("stat zed threads db %s: %w", root.ThreadsDBPath, err)
	}

	db, err := zedstore.OpenReadOnlyDatabase(ctx, root.ThreadsDBPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.open_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, fmt.Errorf("open zed threads db %s: %w", root.ThreadsDBPath, err)
	}
	defer func() { _ = db.Close() }()

	rows, err := zedstore.ReadThreadRows(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.read_threads_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, fmt.Errorf("read zed thread rows from %s: %w", root.ThreadsDBPath, err)
	}

	return rows, nil
}

func readMetadataForRoot(ctx context.Context, root zedstore.DataRoot) (map[string]metadataWithChannel, error) {
	metadataBySession := make(map[string]metadataWithChannel)
	for _, dbPath := range root.MetadataDBPaths {
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.WarnContext(ctx, "providers.zed.parser.stat_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			return nil, fmt.Errorf("stat zed metadata db %s: %w", dbPath, err)
		}

		db, err := zedstore.OpenReadOnlyDatabase(ctx, dbPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.open_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			return nil, fmt.Errorf("open zed metadata db %s: %w", dbPath, err)
		}

		metadataRows, err := zedstore.ReadSidebarThreads(ctx, db)
		_ = db.Close()
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.read_metadata_failed", "concern", concern, "path", dbPath, "err", err)
			return nil, fmt.Errorf("read zed sidebar rows from %s: %w", dbPath, err)
		}

		channel := filepath.Base(filepath.Dir(dbPath))
		for _, row := range metadataRows {
			current, ok := metadataBySession[row.SessionID]
			if !ok || row.UpdatedAt.After(current.Metadata.UpdatedAt) {
				metadataBySession[row.SessionID] = metadataWithChannel{Metadata: row, Channel: channel}
			}
		}
	}

	return metadataBySession, nil
}

func parsableThreadRows(ctx context.Context, path string, rows []zedstore.ThreadRow) []zedstore.ThreadRow {
	filtered := make([]zedstore.ThreadRow, 0, len(rows))
	for _, row := range rows {
		if _, err := zedstore.ParseThreadDocument(row.DataType, row.Data); err == nil {
			filtered = append(filtered, row)
			continue
		} else {
			slog.WarnContext(ctx, "providers.zed.parser.parse_thread_payload_failed", "concern", concern, "path", path, "session_id", row.SessionID, "data_type", string(row.DataType), "err", err)
		}
	}
	return filtered
}

func buildLineage(parentSessionID string, subagentContext *zedstore.SubagentContext) *conversation.Lineage {
	parentNativeID := strings.TrimSpace(parentSessionID)
	if subagentContext != nil && strings.TrimSpace(subagentContext.ParentThreadID) != "" {
		parentNativeID = strings.TrimSpace(subagentContext.ParentThreadID)
	}
	if parentNativeID == "" {
		return nil
	}
	return &conversation.Lineage{
		Kind:              conversation.ConversationLineageKindSpawn,
		ParentProvider:    providerid.ProviderZed,
		ParentNativeID:    parentNativeID,
		ParentMessageUUID: "",
	}
}

func resolvedTitle(metadata zedstore.SidebarThreadMetadata, thread zedstore.ThreadDocument) string {
	title := firstNonEmptyString(metadata.TitleOverride, metadata.Title, thread.Title)
	if title == "" {
		return "Untitled Zed Thread"
	}
	return title
}

func resolvedModel(model *zedstore.ThreadModel) string {
	if model == nil {
		return ""
	}
	if model.Provider == "" || model.Model == "" {
		return ""
	}
	return model.Provider + "/" + model.Model
}

func firstPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func transcriptMessage(thread zedstore.ThreadDocument, message zedstore.ThreadMessage, opts conversation.LoadOptions) (transcript.Message, bool) {
	switch message.Kind {
	case zedstore.ThreadMessageKindUser:
		if message.User == nil {
			return emptyMessage(), false
		}
		return transcript.Message{
			UUID:              message.User.ID,
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "user",
			Visibility:        transcript.MessageVisibilityVisible,
			Compaction:        nil,
			Timestamp:         thread.UpdatedAt,
			Text:              userMessageText(message.User),
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
		}, true
	case zedstore.ThreadMessageKindAgent:
		if message.Agent == nil {
			return emptyMessage(), false
		}
		text, thinking, tools := agentMessageParts(message.Agent, opts.IncludeToolOutputs)
		return transcript.Message{
			UUID:              "",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "assistant",
			Visibility:        transcript.MessageVisibilityVisible,
			Compaction:        nil,
			Timestamp:         thread.UpdatedAt,
			Text:              text,
			Thinking:          thinking,
			HasTools:          len(tools) > 0,
			Tools:             tools,
		}, true
	case zedstore.ThreadMessageKindResume:
		return transcript.Message{
			UUID:              "",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "user",
			Visibility:        transcript.MessageVisibilityVisible,
			Compaction:        nil,
			Timestamp:         thread.UpdatedAt,
			Text:              "Continue where you left off",
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
		}, true
	case zedstore.ThreadMessageKindCompaction:
		if !opts.IncludeSystemMessages || message.Compaction == nil {
			return emptyMessage(), false
		}
		compaction, text := compactionMetadata(message.Compaction)
		return transcript.Message{
			UUID:              "",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "user",
			Visibility:        transcript.MessageVisibilityTranscriptOnly,
			Compaction:        compaction,
			Timestamp:         thread.UpdatedAt,
			Text:              text,
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
		}, true
	default:
		return emptyMessage(), false
	}
}

func userMessageText(message *zedstore.UserMessage) string {
	parts := make([]string, 0, len(message.Content))
	for _, part := range message.Content {
		switch part.Kind {
		case zedstore.UserContentKindText:
			if strings.TrimSpace(part.Text) != "" {
				parts = append(parts, part.Text)
			}
		case zedstore.UserContentKindMention:
			if part.Mention == nil {
				continue
			}
			if strings.TrimSpace(part.Mention.Content) != "" {
				parts = append(parts, part.Mention.Content)
			} else if strings.TrimSpace(part.Mention.URI) != "" {
				parts = append(parts, part.Mention.URI)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func agentMessageParts(message *zedstore.AgentMessage, includeToolOutputs bool) (string, string, []transcript.ToolCall) {
	textParts := make([]string, 0, len(message.Content))
	thinkingParts := make([]string, 0, len(message.Content))
	tools := make([]transcript.ToolCall, 0)
	for _, part := range message.Content {
		switch part.Kind {
		case zedstore.AgentContentKindText:
			if strings.TrimSpace(part.Text) != "" {
				textParts = append(textParts, part.Text)
			}
		case zedstore.AgentContentKindThinking, zedstore.AgentContentKindRedactedThinking:
			if strings.TrimSpace(part.Text) != "" {
				thinkingParts = append(thinkingParts, part.Text)
			}
		case zedstore.AgentContentKindToolUse:
			if part.ToolUse == nil {
				continue
			}
			output, isError := "", false
			if includeToolOutputs {
				output, isError = zedToolResultOutput(message.ToolResults[part.ToolUse.ID])
			}
			tools = append(tools, transcript.ToolCall{
				ID:      part.ToolUse.ID,
				Name:    part.ToolUse.Name,
				Input:   transcript.ToolInputJSON{Raw: append([]byte(nil), part.ToolUse.Input...)},
				Output:  output,
				IsError: isError,
			})
		}
	}
	return strings.Join(textParts, "\n"), strings.Join(thinkingParts, "\n"), tools
}

func zedToolResultOutput(result zedstore.ToolResult) (string, bool) {
	for _, part := range result.Content {
		var text string
		if err := json.Unmarshal(part, &text); err == nil && strings.TrimSpace(text) != "" {
			return text, result.IsError
		}
	}
	if len(result.Output) > 0 && string(result.Output) != "null" {
		return string(result.Output), result.IsError
	}
	return "", result.IsError
}

func compactionMetadata(message *zedstore.CompactionMessage) (*transcript.CompactionMetadata, string) {
	switch message.Kind {
	case zedstore.CompactionMessageKindSummary:
		return &transcript.CompactionMetadata{
			Kind:                      transcript.CompactionKindSummary,
			Trigger:                   transcript.CompactionTriggerUnknown,
			PreTokens:                 0,
			PostTokens:                0,
			TokensSaved:               0,
			MessagesSummarized:        0,
			ReplacementHistoryCount:   0,
			HeadUUID:                  "",
			AnchorUUID:                "",
			TailUUID:                  "",
			ContextItems:              nil,
			UserContext:               "",
			Direction:                 "",
			PreCompactDiscoveredTools: nil,
			CompactedToolIDs:          nil,
			ClearedAttachmentUUIDs:    nil,
			RawCompactMetadata:        nil,
			RawMicrocompactMetadata:   nil,
			RawSummarizeMetadata:      nil,
		}, message.Summary
	case zedstore.CompactionMessageKindProviderNative:
		raw, err := json.Marshal(message.Items)
		if err != nil {
			return nil, ""
		}
		return &transcript.CompactionMetadata{
			Kind:                      transcript.CompactionKindBoundary,
			Trigger:                   transcript.CompactionTriggerUnknown,
			PreTokens:                 0,
			PostTokens:                0,
			TokensSaved:               0,
			MessagesSummarized:        0,
			ReplacementHistoryCount:   0,
			HeadUUID:                  "",
			AnchorUUID:                "",
			TailUUID:                  "",
			ContextItems:              nil,
			UserContext:               "",
			Direction:                 "",
			PreCompactDiscoveredTools: nil,
			CompactedToolIDs:          nil,
			ClearedAttachmentUUIDs:    nil,
			RawCompactMetadata:        raw,
			RawMicrocompactMetadata:   nil,
			RawSummarizeMetadata:      nil,
		}, ""
	default:
		return nil, ""
	}
}
