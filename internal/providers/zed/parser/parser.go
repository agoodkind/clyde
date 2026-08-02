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
	artifactKindZedTerm   = "zed_terminal_thread"
	resolveLookupTimeout  = 5 * time.Second
	terminalPathPrefix    = "terminal-"
)

type discoveredThread struct {
	Row      zedstore.ThreadRow
	Thread   zedstore.ThreadDocument
	Metadata zedstore.SidebarThreadMetadata
	Terminal *zedstore.SidebarTerminalThreadMetadata
	Source   string
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
		terminalMetadata := readTerminalMetadataForRoot(ctx, root)
		if len(rows) == 0 && len(terminalMetadata) == 0 {
			continue
		}
		metadataByThread := readMetadataForRoot(ctx, root)
		rootHash := RootHash(root.RootDir)

		for _, row := range rows {
			// A thread whose sidebar metadata names an agent is a subagent thread.
			// Discovery admits it and ScanRecord classifies it, so the one
			// conversation setting decides whether it is served. Dropping it here
			// instead would be a second, always-on skip the setting could not reach.
			metadata, ok := metadataByThread[row.ThreadID]
			if !ok {
				continue
			}
			thread, err := zedstore.ParseThreadDocument(row.DataType, row.Data)
			if err != nil {
				slog.WarnContext(ctx, "providers.zed.parser.discover_parse_thread_payload_failed", "concern", concern, "component", "zed", "path", root.ThreadsDBPath, "thread_id", row.ThreadID, "data_type", string(row.DataType), "err", err)
				continue
			}
			path := buildVirtualPathFromRootHash(rootHash, metadata.Channel, row.ThreadID)
			if path == "" {
				continue
			}
			candidates = append(candidates, conversation.ScanCandidate{
				Path:  path,
				Stamp: conversation.FileStamp{Size: int64(len(row.Data)), Mtime: row.UpdatedAt},
			})
			discovered[path] = newNativeDiscoveredThread(row, thread, metadata.Metadata, root.RootDir, metadata.Channel)
		}

		for _, terminal := range terminalMetadata {
			path := buildVirtualPathFromRootHash(rootHash, terminal.Channel, terminalSessionID(terminal.Metadata.TerminalID))
			if path == "" {
				continue
			}
			candidates = append(candidates, conversation.ScanCandidate{
				Path:  path,
				Stamp: terminalMetadataStamp(terminal.Metadata),
			})
			discovered[path] = newTerminalDiscoveredThread(terminal.Metadata, root.RootDir, terminal.Channel)
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
// record without streaming the full transcript. Zed threads carry a subagent
// context, and both are read into the record's origin so the one conversation
// setting decides whether a subagent thread is served.
func (p *Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	discovered, err := p.resolveDiscoveredThread(path)
	if err != nil {
		return emptyRecord(), false
	}
	if discovered.Source == artifactKindZedTerm {
		if discovered.Terminal == nil {
			return emptyRecord(), false
		}
		terminal := discovered.Terminal
		title := firstNonEmptyString(terminal.CustomTitle, terminal.Title)
		if title == "" {
			title = "Untitled Zed Terminal"
		}
		return conversation.Record{
			ID:              conversation.DerivedID(providerid.ProviderZed, terminalSessionID(terminal.TerminalID), path),
			Provider:        providerid.ProviderZed,
			NativeID:        terminal.TerminalID,
			Lineage:         nil,
			Origin:          conversation.OriginUnspecified,
			Title:           title,
			TitleUncertain:  false,
			WorkspaceRoot:   firstNonEmptyString(terminal.WorkingDirectory, firstPath(terminal.FolderPaths), firstPath(terminal.MainWorktreePaths)),
			ArtifactPath:    path,
			ArtifactKind:    artifactKindZedTerm,
			Model:           "",
			CreatedAt:       terminal.CreatedAt,
			UpdatedAt:       terminal.CreatedAt,
			SizeBytes:       0,
			Archived:        false,
			LatestRequestID: "",
		}, true
	}

	thread := discovered.Thread
	return conversation.Record{
		ID:             conversation.DerivedID(providerid.ProviderZed, discovered.Row.ThreadID, path),
		Provider:       providerid.ProviderZed,
		NativeID:       discovered.Row.ThreadID,
		Lineage:        buildLineage(discovered.Row.ParentThreadID, thread.SubagentContext),
		Origin:         zedThreadOrigin(discovered.Metadata.AgentID, thread.SubagentContext),
		Title:          resolvedTitle(discovered.Metadata, thread),
		TitleUncertain: false,
		WorkspaceRoot:  firstNonEmptyString(firstPath(discovered.Metadata.FolderPaths), firstPath(discovered.Row.FolderPaths)),
		ArtifactPath:   path,
		ArtifactKind:   artifactKindZedThread,
		Model:          resolvedModel(thread.Model),
		CreatedAt:      firstNonZeroTime(discovered.Metadata.CreatedAt, discovered.Row.CreatedAt),
		UpdatedAt:      latestNonZeroTime(discovered.Metadata.UpdatedAt, discovered.Row.UpdatedAt, thread.UpdatedAt),
		SizeBytes:      stamp.Size,
		Archived:       discovered.Metadata.Archived,
		// Zed thread headers carry no request id.
		LatestRequestID: "",
	}, true
}

// Stream lazily yields transcript-shaped messages for one discovered Zed
// thread.
func (p *Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		discovered, err := p.resolveDiscoveredThread(path)
		if err != nil {
			yield(emptyMessage(), err)
			return
		}
		if discovered.Source == artifactKindZedTerm {
			if !opts.IncludeSystemMessages || discovered.Terminal == nil {
				return
			}
			yield(transcript.Message{
				UUID:              "",
				ParentUUID:        "",
				LogicalParentUUID: "",
				Role:              "system",
				Visibility:        transcript.MessageVisibilityMetaOnly,
				Compaction:        nil,
				Timestamp:         discovered.Terminal.CreatedAt,
				Text:              terminalMetadataUnavailableText(*discovered.Terminal),
				Thinking:          "",
				HasTools:          false,
				Tools:             nil,
				HarnessStrips:     transcript.HarnessStrips{Injected: 0, System: 0},
			}, nil)
			return
		}
		thread := discovered.Thread
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
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "",
		Visibility:        "",
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              "",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
		HarnessStrips:     transcript.HarnessStrips{Injected: 0, System: 0},
	}
}

func readThreadRowsForRoot(ctx context.Context, root zedstore.DataRoot) ([]zedstore.ThreadRow, error) {
	if _, err := os.Stat(root.ThreadsDBPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		slog.WarnContext(ctx, "providers.zed.parser.stat_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, nil
	}

	db, err := zedstore.OpenReadOnlyDatabase(ctx, root.ThreadsDBPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.open_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, nil
	}
	defer func() { _ = db.Close() }()

	rows, err := zedstore.ReadThreadRows(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.read_threads_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, nil
	}
	return rows, nil
}

func readMetadataForRoot(ctx context.Context, root zedstore.DataRoot) map[string]metadataWithChannel {
	metadataByThread := make(map[string]metadataWithChannel)
	for _, dbPath := range root.MetadataDBPaths {
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.WarnContext(ctx, "providers.zed.parser.stat_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			continue
		}

		db, err := zedstore.OpenReadOnlyDatabase(ctx, dbPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.open_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			continue
		}

		metadataRows, err := zedstore.ReadSidebarThreads(ctx, db)
		_ = db.Close()
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.read_metadata_failed", "concern", concern, "path", dbPath, "err", err)
			continue
		}

		channel := filepath.Base(filepath.Dir(dbPath))
		for _, row := range metadataRows {
			current, ok := metadataByThread[row.SessionID]
			if !ok || row.UpdatedAt.After(current.Metadata.UpdatedAt) {
				metadataByThread[row.SessionID] = metadataWithChannel{Metadata: row, Channel: channel}
			}
		}
	}
	return metadataByThread
}

func (p *Parser) resolveDiscoveredThread(path string) (discoveredThread, error) {
	var emptyDiscovered discoveredThread

	p.mu.Lock()
	discovered, ok := p.discovered[path]
	p.mu.Unlock()
	if ok {
		return discovered, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveLookupTimeout)
	defer cancel()

	virtualPath, err := ParseVirtualPath(path)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.virtual_path_parse_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscovered, fmt.Errorf("parse zed virtual path: %w", err)
	}
	roots, err := zedstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.resolve_roots_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscovered, fmt.Errorf("resolve zed data roots: %w", err)
	}

	var firstLookupErr error
	for _, root := range roots {
		if RootHash(root.RootDir) != virtualPath.RootHash {
			continue
		}
		terminalID, isTerminal := strings.CutPrefix(virtualPath.SessionID, terminalPathPrefix)
		if isTerminal {
			resolved, ok, err := resolveTerminalDiscoveredThread(ctx, root, virtualPath.Channel, terminalID)
			if err != nil {
				if firstLookupErr == nil {
					firstLookupErr = err
				}
				continue
			}
			if !ok {
				continue
			}
			p.mu.Lock()
			p.discovered[path] = resolved
			p.mu.Unlock()
			return resolved, nil
		}
		resolved, ok, err := resolveNativeDiscoveredThread(ctx, root, virtualPath.Channel, virtualPath.SessionID)
		if err != nil {
			if firstLookupErr == nil {
				firstLookupErr = err
			}
			continue
		}
		if !ok {
			continue
		}
		p.mu.Lock()
		p.discovered[path] = resolved
		p.mu.Unlock()
		return resolved, nil
	}
	if firstLookupErr != nil {
		return emptyDiscovered, firstLookupErr
	}
	slog.WarnContext(ctx, "providers.zed.parser.virtual_path_not_found", "concern", concern, "path", path, "session_id", virtualPath.SessionID, "channel", virtualPath.Channel)
	return emptyDiscovered, fmt.Errorf("zed path not discovered: %s", path)
}

func readThreadRowByIDForRoot(ctx context.Context, root zedstore.DataRoot, threadID string) (zedstore.ThreadRow, bool, error) {
	var emptyRow zedstore.ThreadRow

	if _, err := os.Stat(root.ThreadsDBPath); err != nil {
		if os.IsNotExist(err) {
			return emptyRow, false, nil
		}
		slog.WarnContext(ctx, "providers.zed.parser.stat_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "thread_id", threadID, "err", err)
		return emptyRow, false, fmt.Errorf("stat zed threads db %s: %w", root.ThreadsDBPath, err)
	}

	db, err := zedstore.OpenReadOnlyDatabase(ctx, root.ThreadsDBPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.open_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "thread_id", threadID, "err", err)
		return emptyRow, false, fmt.Errorf("open zed threads db %s: %w", root.ThreadsDBPath, err)
	}
	defer func() { _ = db.Close() }()

	row, ok, err := zedstore.ReadThreadRowByID(ctx, db, threadID)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.read_thread_row_by_id_failed", "concern", concern, "path", root.ThreadsDBPath, "thread_id", threadID, "err", err)
		return emptyRow, false, fmt.Errorf("read zed thread row %q from %s: %w", threadID, root.ThreadsDBPath, err)
	}
	return row, ok, nil
}

func readSidebarThreadMetadataForRoot(
	ctx context.Context,
	root zedstore.DataRoot,
	sessionID string,
	channel string,
) (metadataWithChannel, bool, error) {
	var emptyMetadata metadataWithChannel

	for _, dbPath := range root.MetadataDBPaths {
		dbChannel := filepath.Base(filepath.Dir(dbPath))
		if dbChannel != channel {
			continue
		}
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.WarnContext(ctx, "providers.zed.parser.stat_metadata_db_failed", "concern", concern, "path", dbPath, "session_id", sessionID, "err", err)
			return emptyMetadata, false, fmt.Errorf("stat zed metadata db %s: %w", dbPath, err)
		}

		db, err := zedstore.OpenReadOnlyDatabase(ctx, dbPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.open_metadata_db_failed", "concern", concern, "path", dbPath, "session_id", sessionID, "err", err)
			return emptyMetadata, false, fmt.Errorf("open zed metadata db %s: %w", dbPath, err)
		}
		row, ok, err := zedstore.ReadSidebarThreadBySession(ctx, db, sessionID)
		_ = db.Close()
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.read_sidebar_thread_by_session_failed", "concern", concern, "path", dbPath, "session_id", sessionID, "err", err)
			return emptyMetadata, false, fmt.Errorf("read zed sidebar row %q from %s: %w", sessionID, dbPath, err)
		}
		if ok {
			return metadataWithChannel{Metadata: row, Channel: dbChannel}, true, nil
		}
	}
	return emptyMetadata, false, nil
}

// zedThreadOrigin classifies a Zed thread from the two markers Zed writes: the
// sidebar metadata names the agent that owns the thread, and the thread document
// persists a subagent context naming the parent. Either one marks a thread Zed
// itself dispatched. Before the conversation setting existed, a thread with an
// agent id was dropped during discovery instead, which the setting could not
// reach.
func zedThreadOrigin(agentID string, subagentContext *zedstore.SubagentContext) conversation.Origin {
	if strings.TrimSpace(agentID) != "" {
		return conversation.OriginSubagent
	}
	if subagentContext != nil {
		return conversation.OriginSubagent
	}
	return conversation.OriginUser
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
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
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

func latestNonZeroTime(values ...time.Time) time.Time {
	latest := time.Time{}
	for _, value := range values {
		if value.IsZero() {
			continue
		}
		if latest.IsZero() || value.After(latest) {
			latest = value
		}
	}
	return latest
}

func transcriptMessage(thread zedstore.ThreadDocument, message zedstore.ThreadMessage, opts conversation.LoadOptions) (transcript.Message, bool) {
	defer transcript.ContainMappingPanic()
	switch message.Kind {
	case zedstore.ThreadMessageKindUser:
		if message.User == nil {
			return emptyMessage(), false
		}
		text := userMessageText(message.User)
		if strings.TrimSpace(text) == "" {
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
			Text:              text,
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
			HarnessStrips:     transcript.HarnessStrips{Injected: 0, System: 0},
		}, true
	case zedstore.ThreadMessageKindAgent:
		if message.Agent == nil {
			return emptyMessage(), false
		}
		text, thinking, tools := agentMessageParts(message.Agent, opts.IncludeToolOutputs)
		if text == "" && thinking == "" && len(tools) == 0 {
			return emptyMessage(), false
		}
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
			HarnessStrips:     transcript.HarnessStrips{Injected: 0, System: 0},
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
			HarnessStrips:     transcript.HarnessStrips{Injected: 0, System: 0},
		}, true
	case zedstore.ThreadMessageKindCompaction:
		if !opts.IncludeSystemMessages || message.Compaction == nil {
			return emptyMessage(), false
		}
		compaction, text := compactionMetadata(message.Compaction)
		if compaction == nil || strings.TrimSpace(text) == "" {
			return emptyMessage(), false
		}
		return transcript.Message{
			UUID:              "",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "system",
			Visibility:        transcript.MessageVisibilityTranscriptOnly,
			Compaction:        compaction,
			Timestamp:         thread.UpdatedAt,
			Text:              text,
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
			HarnessStrips:     transcript.HarnessStrips{Injected: 0, System: 0},
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
			input := transcript.ToolInputJSON{Raw: append([]byte(nil), part.ToolUse.Input...)}
			display, displayLang := toolDisplayText(part.ToolUse.Name, input)
			tools = append(tools, transcript.ToolCall{
				ID:          part.ToolUse.ID,
				Name:        part.ToolUse.Name,
				Input:       input,
				Display:     display,
				DisplayLang: displayLang,
				Output:      output,
				IsError:     isError,
			})
		}
	}
	return strings.Join(textParts, "\n"), strings.Join(thinkingParts, "\n"), tools
}

func zedToolResultOutput(result zedstore.ToolResult) (string, bool) {
	textParts := make([]string, 0, len(result.Content))
	for _, part := range result.Content {
		var text string
		if err := json.Unmarshal(part, &text); err == nil {
			if strings.TrimSpace(text) == "" {
				continue
			}
			textParts = append(textParts, text)
			continue
		}
		trimmedPart := strings.TrimSpace(string(part))
		if trimmedPart != "" && trimmedPart != "null" {
			textParts = append(textParts, trimmedPart)
		}
	}
	if outputText := zedToolResultFallbackOutput(result.Output); outputText != "" {
		textParts = append(textParts, outputText)
	}
	if len(textParts) > 0 {
		return strings.Join(textParts, "\n"), result.IsError
	}
	return "", result.IsError
}

func zedToolResultFallbackOutput(raw json.RawMessage) string {
	trimmedOutput := strings.TrimSpace(string(raw))
	if trimmedOutput == "" || trimmedOutput == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return ""
		}
		return text
	}
	return trimmedOutput
}

func compactionMetadata(message *zedstore.CompactionMessage) (*transcript.CompactionMetadata, string) {
	switch message.Kind {
	case zedstore.CompactionMessageKindSummary:
		summary := strings.TrimSpace(message.Summary)
		if summary == "" {
			return nil, ""
		}
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
		}, summary
	case zedstore.CompactionMessageKindProviderNative:
		raw, err := json.Marshal(message.Items)
		if err != nil {
			raw = nil
		}
		text := "Conversation compacted"
		if provider := strings.TrimSpace(message.Provider); provider != "" {
			text = provider + " compaction boundary"
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
		}, text
	default:
		return nil, ""
	}
}
