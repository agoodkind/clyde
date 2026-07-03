// Package parser wires Cursor transcript and SQLite artifacts into Clyde's
// conversation index.
package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	"goodkind.io/clyde/internal/transcript"
)

const (
	concern                        = "providers.cursor.parser"
	artifactKindCursorAgent        = "cursor_agent_transcript"
	artifactKindCursorComposer     = "cursor_composer"
	artifactKindCursorBackground   = "cursor_background_composer"
	artifactKindCursorLegacyChat   = "cursor_legacy_chat"
	discoveredKindJSONL            = "jsonl"
	discoveredKindComposer         = "composer"
	discoveredKindLegacy           = "legacy"
	resolveLookupTimeout           = 5 * time.Second
	untitledCursorConversationText = "Untitled Cursor Conversation"
	untitledCursorChatText         = "Untitled Cursor Chat"
	maxTitleRunes                  = 80
)

type discoveredArtifact struct {
	Kind           string
	Path           string
	ConversationID string
	ProjectKey     string
	RootDir        string
	ComposerID     string
	ComposerHeader cursorstore.ComposerHeader
	ComposerInfo   cursorstore.WorkspaceComposerInfo
	HasInfo        bool
	IsBackground   bool
	LegacyTab      cursorstore.LegacyChatTab
	WorkspaceRoot  string
}

// Parser discovers Cursor conversation artifacts and caches the rows needed by
// later scan stages.
type Parser struct {
	mu         sync.Mutex
	discovered map[string]discoveredArtifact
}

var _ conversation.Parser = (*Parser)(nil)

// New returns a Cursor conversation parser.
func New() *Parser {
	return &Parser{mu: sync.Mutex{}, discovered: make(map[string]discoveredArtifact)}
}

// Provider reports that this parser handles Cursor artifacts.
func (*Parser) Provider() providerid.Provider {
	return providerid.ProviderCursor
}

// Discover resolves local Cursor transcript and SQLite data roots, then returns
// scan candidates for modern JSONL transcripts, composers, and legacy chats.
func (p *Parser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	candidates := make([]conversation.ScanCandidate, 0)
	discovered := make(map[string]discoveredArtifact)
	seenConversationIDs := make(map[string]bool)

	jsonlCandidates, err := discoverJSONL(ctx, discovered, seenConversationIDs)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, jsonlCandidates...)

	sqliteCandidates, err := discoverSQLite(ctx, discovered, seenConversationIDs)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, sqliteCandidates...)

	sort.SliceStable(candidates, func(i int, j int) bool {
		return candidates[i].Path < candidates[j].Path
	})

	p.mu.Lock()
	p.discovered = discovered
	p.mu.Unlock()

	return candidates, nil
}

// ScanRecord turns one discovered Cursor artifact into a derived Clyde record
// without streaming the full transcript.
func (p *Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	discovered, err := p.resolveDiscovered(path)
	if err != nil {
		slog.Warn("providers.cursor.parser.scan_resolve_failed", "concern", concern, "path", path, "err", err)
		return emptyRecord(), false
	}
	switch discovered.Kind {
	case discoveredKindJSONL:
		header, err := cursorjsonl.ScanHeader(path)
		if err != nil {
			slog.Warn("providers.cursor.parser.scan_header_failed", "concern", concern, "path", path, "err", err)
			return emptyRecord(), false
		}
		return conversation.Record{
			ID:            conversation.DerivedID(providerid.ProviderCursor, discovered.ConversationID, path),
			Provider:      providerid.ProviderCursor,
			NativeID:      discovered.ConversationID,
			Lineage:       nil,
			Title:         firstNonEmptyString(truncateTitle(header.FirstUserText), untitledCursorConversationText),
			WorkspaceRoot: cursorjsonl.WorkspacePathFromProjectKey(discovered.ProjectKey),
			ArtifactPath:  path,
			ArtifactKind:  artifactKindCursorAgent,
			Model:         "",
			CreatedAt:     stamp.Mtime,
			UpdatedAt:     stamp.Mtime,
			SizeBytes:     stamp.Size,
			Archived:      false,
		}, true
	case discoveredKindComposer:
		header := discovered.ComposerHeader
		workspaceTitle := ""
		workspaceRoot := ""
		archived := false
		if discovered.HasInfo {
			workspaceTitle = discovered.ComposerInfo.Name
			workspaceRoot = discovered.ComposerInfo.Cwd
			archived = discovered.ComposerInfo.Archived
		}
		return conversation.Record{
			ID:            conversation.DerivedID(providerid.ProviderCursor, discovered.ComposerID, path),
			Provider:      providerid.ProviderCursor,
			NativeID:      discovered.ComposerID,
			Lineage:       nil,
			Title:         firstNonEmptyString(header.Name, workspaceTitle, untitledCursorConversationText),
			WorkspaceRoot: workspaceRoot,
			ArtifactPath:  path,
			ArtifactKind:  composerArtifactKind(discovered.IsBackground),
			Model:         "",
			CreatedAt:     msToTime(header.CreatedAt),
			UpdatedAt:     msToTime(header.LastUpdatedAt),
			SizeBytes:     0,
			Archived:      archived,
		}, true
	case discoveredKindLegacy:
		tab := discovered.LegacyTab
		virtualPath, err := ParseVirtualPath(path)
		if err != nil {
			return emptyRecord(), false
		}
		legacyConversationID := virtualPath.ID
		return conversation.Record{
			ID:            conversation.DerivedID(providerid.ProviderCursor, legacyConversationID, path),
			Provider:      providerid.ProviderCursor,
			NativeID:      legacyConversationID,
			Lineage:       nil,
			Title:         firstNonEmptyString(tab.ChatTitle, untitledCursorChatText),
			WorkspaceRoot: discovered.WorkspaceRoot,
			ArtifactPath:  path,
			ArtifactKind:  artifactKindCursorLegacyChat,
			Model:         "",
			CreatedAt:     stamp.Mtime,
			UpdatedAt:     stamp.Mtime,
			SizeBytes:     0,
			Archived:      false,
		}, true
	default:
		return emptyRecord(), false
	}
}

// Stream lazily yields transcript-shaped messages for one discovered Cursor
// artifact.
func (p *Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		discovered, err := p.resolveDiscovered(path)
		if err != nil {
			yield(emptyMessage(), err)
			return
		}
		switch discovered.Kind {
		case discoveredKindJSONL:
			err := cursorjsonl.StreamMessages(path, func(message cursorjsonl.TranscriptMessage) error {
				mapped, include := mapJSONLMessage(message, opts)
				if !include {
					return nil
				}
				if !yield(mapped, nil) {
					return cursorjsonl.ErrStopStreaming
				}
				return nil
			})
			if err != nil && !errors.Is(err, cursorjsonl.ErrStopStreaming) {
				yield(emptyMessage(), err)
			}
		case discoveredKindComposer:
			streamComposer(discovered, opts, yield)
		case discoveredKindLegacy:
			for _, bubble := range discovered.LegacyTab.Bubbles {
				mapped, include := mapLegacyBubble(bubble, opts)
				if !include {
					continue
				}
				if !yield(mapped, nil) {
					return
				}
			}
		}
	}
}

func discoverJSONL(
	ctx context.Context,
	discovered map[string]discoveredArtifact,
	seenConversationIDs map[string]bool,
) ([]conversation.ScanCandidate, error) {
	roots, err := cursorjsonl.ResolveProjectRoots()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_project_roots_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("resolve cursor project roots: %w", err)
	}
	files, err := cursorjsonl.DiscoverTranscriptFiles(roots)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.discover_transcripts_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("discover cursor transcript files: %w", err)
	}

	candidates := make([]conversation.ScanCandidate, 0, len(files))
	for _, file := range files {
		info, err := os.Stat(file.Path)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.stat_jsonl_failed", "concern", concern, "path", file.Path, "err", err)
			continue
		}
		candidates = append(candidates, conversation.ScanCandidate{
			Path: file.Path,
			Stamp: conversation.FileStamp{
				Size:  info.Size(),
				Mtime: info.ModTime(),
			},
		})
		var artifact discoveredArtifact
		artifact.Kind = discoveredKindJSONL
		artifact.Path = file.Path
		artifact.ConversationID = file.ConversationID
		artifact.ProjectKey = file.ProjectKey
		discovered[file.Path] = artifact
		seenConversationIDs[file.ConversationID] = true
	}
	return candidates, nil
}

func discoverSQLite(
	ctx context.Context,
	discovered map[string]discoveredArtifact,
	seenConversationIDs map[string]bool,
) ([]conversation.ScanCandidate, error) {
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_data_roots_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("resolve cursor data roots: %w", err)
	}

	candidates := make([]conversation.ScanCandidate, 0)
	for _, root := range roots {
		candidates = append(candidates, discoverComposersForRoot(ctx, root, discovered, seenConversationIDs)...)
		candidates = append(candidates, discoverLegacyForRoot(ctx, root, discovered)...)
	}
	return candidates, nil
}

func discoverComposersForRoot(
	ctx context.Context,
	root cursorstore.DataRoot,
	discovered map[string]discoveredArtifact,
	seenConversationIDs map[string]bool,
) []conversation.ScanCandidate {
	db, ok := openOptionalDatabase(ctx, root.GlobalDBPath, "global_db")
	if !ok {
		return nil
	}
	defer func() { _ = db.Close() }()

	composerIDs, err := cursorstore.ListComposerIDs(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_composers_failed", "concern", concern, "path", root.GlobalDBPath, "err", err)
		return nil
	}
	backgroundIDs := backgroundComposerIDSet(ctx, db, root.GlobalDBPath)
	workspaceIndex := workspaceComposerIndex(ctx, root)
	rootHash := RootHash(root.RootDir)

	candidates := make([]conversation.ScanCandidate, 0, len(composerIDs))
	for _, composerID := range composerIDs {
		if seenConversationIDs[composerID] {
			continue
		}
		header, found, err := cursorstore.ReadComposerHeader(ctx, db, composerID)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.read_composer_header_failed", "concern", concern, "path", root.GlobalDBPath, "composer_id", composerID, "err", err)
			continue
		}
		if !found {
			continue
		}
		if len(header.FullConversationHeadersOnly) == 0 {
			// A composer with no bubble headers has no conversation content to
			// deliver, so it is a draft or empty pane rather than a real
			// conversation. Mirror the legacy-chat guard below and skip it so it
			// never enters the manifest. A composer mid-first-message re-enters
			// discovery once it gains headers.
			continue
		}
		path := BuildVirtualPath(rootHash, VirtualKindComposer, composerID)
		if path == "" {
			continue
		}
		info, hasInfo := workspaceIndex[composerID]
		candidates = append(candidates, conversation.ScanCandidate{
			Path: path,
			Stamp: conversation.FileStamp{
				Size:  int64(len(header.FullConversationHeadersOnly)),
				Mtime: msToTime(header.LastUpdatedAt),
			},
		})
		var artifact discoveredArtifact
		artifact.Kind = discoveredKindComposer
		artifact.Path = path
		artifact.RootDir = root.RootDir
		artifact.ComposerID = composerID
		artifact.ComposerHeader = header
		artifact.ComposerInfo = info
		artifact.HasInfo = hasInfo
		artifact.IsBackground = backgroundIDs[composerID]
		discovered[path] = artifact
	}
	return candidates
}

func discoverLegacyForRoot(
	ctx context.Context,
	root cursorstore.DataRoot,
	discovered map[string]discoveredArtifact,
) []conversation.ScanCandidate {
	entries, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_workspace_entries_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return nil
	}
	rootHash := RootHash(root.RootDir)
	candidates := make([]conversation.ScanCandidate, 0)
	for _, entry := range entries {
		candidates = append(candidates, discoverLegacyForEntry(ctx, rootHash, entry, discovered)...)
	}
	return candidates
}

func discoverLegacyForEntry(
	ctx context.Context,
	rootHash string,
	entry cursorstore.WorkspaceEntry,
	discovered map[string]discoveredArtifact,
) []conversation.ScanCandidate {
	db, ok := openOptionalDatabase(ctx, entry.StateDBPath, "workspace_db")
	if !ok {
		return nil
	}
	defer func() { _ = db.Close() }()

	chatData, found, err := cursorstore.ReadLegacyChatData(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_legacy_chat_failed", "concern", concern, "path", entry.StateDBPath, "workspace_hash", entry.WorkspaceHash, "err", err)
		return nil
	}
	if !found {
		return nil
	}
	workspaceRoot := readWorkspaceRoot(ctx, entry)
	stampMtime := time.Time{}
	stateDBInfo, statErr := os.Stat(entry.StateDBPath)
	if statErr == nil {
		stampMtime = stateDBInfo.ModTime()
	}
	candidates := make([]conversation.ScanCandidate, 0, len(chatData.Tabs))
	for _, tab := range chatData.Tabs {
		if len(tab.Bubbles) == 0 {
			continue
		}
		id := legacyID(entry.WorkspaceHash, tab.TabID)
		path := BuildVirtualPath(rootHash, VirtualKindLegacy, id)
		if path == "" {
			continue
		}
		candidates = append(candidates, conversation.ScanCandidate{
			Path: path,
			Stamp: conversation.FileStamp{
				Size:  int64(len(tab.Bubbles)),
				Mtime: stampMtime,
			},
		})
		var artifact discoveredArtifact
		artifact.Kind = discoveredKindLegacy
		artifact.Path = path
		artifact.LegacyTab = tab
		artifact.WorkspaceRoot = workspaceRoot
		discovered[path] = artifact
	}
	return candidates
}

func streamComposer(
	discovered discoveredArtifact,
	opts conversation.LoadOptions,
	yield func(transcript.Message, error) bool,
) {
	ctx := context.Background()

	dbPath, err := globalDBPathForDiscovered(ctx, discovered)
	if err != nil {
		yield(emptyMessage(), err)
		return
	}
	db, err := cursorstore.OpenReadOnlyDatabase(ctx, dbPath)
	if err != nil {
		yield(emptyMessage(), fmt.Errorf("open cursor global db %s: %w", dbPath, err))
		return
	}
	defer func() { _ = db.Close() }()

	for _, bubbleRef := range discovered.ComposerHeader.FullConversationHeadersOnly {
		bubble, found, err := cursorstore.ReadBubble(ctx, db, discovered.ComposerID, bubbleRef.BubbleID)
		if err != nil {
			yield(emptyMessage(), fmt.Errorf("load cursor composer %q bubble %q: %w", discovered.ComposerID, bubbleRef.BubbleID, err))
			return
		}
		if !found {
			continue
		}
		mapped, include := mapComposerBubble(bubble, opts)
		if !include {
			continue
		}
		if !yield(mapped, nil) {
			return
		}
	}
}

func (p *Parser) resolveDiscovered(path string) (discoveredArtifact, error) {
	var emptyDiscovered discoveredArtifact

	p.mu.Lock()
	discovered, ok := p.discovered[path]
	p.mu.Unlock()
	if ok {
		return discovered, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveLookupTimeout)
	defer cancel()

	var resolved discoveredArtifact
	var err error
	if !strings.HasPrefix(path, virtualPathPrefix) {
		resolved, err = resolveJSONLDiscovered(ctx, path)
	} else {
		resolved, err = resolveVirtualDiscovered(ctx, path)
	}
	if err != nil {
		return emptyDiscovered, err
	}
	p.mu.Lock()
	p.discovered[path] = resolved
	p.mu.Unlock()
	return resolved, nil
}

func resolveJSONLDiscovered(ctx context.Context, path string) (discoveredArtifact, error) {
	if _, err := os.Stat(path); err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.stat_jsonl_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("stat cursor transcript %s: %w", path, err)
	}
	file, ok, err := cursorjsonl.MatchTranscriptFile(path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.match_transcript_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("match cursor transcript file: %w", err)
	}
	if !ok {
		slog.WarnContext(ctx, "providers.cursor.parser.jsonl_path_not_found", "concern", concern, "path", path)
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor transcript path not discovered: %s", path)
	}
	var artifact discoveredArtifact
	artifact.Kind = discoveredKindJSONL
	artifact.Path = file.Path
	artifact.ConversationID = file.ConversationID
	artifact.ProjectKey = file.ProjectKey
	return artifact, nil
}

func resolveVirtualDiscovered(ctx context.Context, path string) (discoveredArtifact, error) {
	virtualPath, err := ParseVirtualPath(path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.virtual_path_parse_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("parse cursor virtual path: %w", err)
	}
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_data_roots_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("resolve cursor data roots: %w", err)
	}
	for _, root := range roots {
		if RootHash(root.RootDir) != virtualPath.RootHash {
			continue
		}
		switch virtualPath.Kind {
		case VirtualKindComposer:
			return resolveComposerDiscovered(ctx, root, path, virtualPath.ID)
		case VirtualKindLegacy:
			return resolveLegacyDiscovered(ctx, root, path, virtualPath.ID)
		}
	}
	slog.WarnContext(ctx, "providers.cursor.parser.virtual_path_not_found", "concern", concern, "path", path, "kind", virtualPath.Kind, "id", virtualPath.ID)
	return emptyDiscoveredArtifact(), fmt.Errorf("cursor path not discovered: %s", path)
}

func resolveComposerDiscovered(
	ctx context.Context,
	root cursorstore.DataRoot,
	path string,
	composerID string,
) (discoveredArtifact, error) {
	db, ok := openOptionalDatabase(ctx, root.GlobalDBPath, "global_db")
	if !ok {
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor global db not found: %s", root.GlobalDBPath)
	}
	defer func() { _ = db.Close() }()

	header, found, err := cursorstore.ReadComposerHeader(ctx, db, composerID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_composer_header_failed", "concern", concern, "path", root.GlobalDBPath, "composer_id", composerID, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("read cursor composer header %q: %w", composerID, err)
	}
	if !found {
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor composer %q not found", composerID)
	}
	workspaceIndex := workspaceComposerIndex(ctx, root)
	info, hasInfo := workspaceIndex[composerID]
	backgroundIDs := backgroundComposerIDSet(ctx, db, root.GlobalDBPath)
	var artifact discoveredArtifact
	artifact.Kind = discoveredKindComposer
	artifact.Path = path
	artifact.RootDir = root.RootDir
	artifact.ComposerID = composerID
	artifact.ComposerHeader = header
	artifact.ComposerInfo = info
	artifact.HasInfo = hasInfo
	artifact.IsBackground = backgroundIDs[composerID]
	return artifact, nil
}

func resolveLegacyDiscovered(
	ctx context.Context,
	root cursorstore.DataRoot,
	path string,
	id string,
) (discoveredArtifact, error) {
	workspaceHash, tabID, ok := splitLegacyID(id)
	if !ok {
		return emptyDiscoveredArtifact(), fmt.Errorf("invalid cursor legacy id %q", id)
	}
	entries, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_workspace_entries_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("list cursor workspace entries: %w", err)
	}
	for _, entry := range entries {
		if entry.WorkspaceHash != workspaceHash {
			continue
		}
		return resolveLegacyDiscoveredForEntry(ctx, path, tabID, entry)
	}
	return emptyDiscoveredArtifact(), fmt.Errorf("cursor workspace %q not found", workspaceHash)
}

func resolveLegacyDiscoveredForEntry(
	ctx context.Context,
	path string,
	tabID string,
	entry cursorstore.WorkspaceEntry,
) (discoveredArtifact, error) {
	db, ok := openOptionalDatabase(ctx, entry.StateDBPath, "workspace_db")
	if !ok {
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor workspace db not found: %s", entry.StateDBPath)
	}
	defer func() { _ = db.Close() }()

	chatData, found, err := cursorstore.ReadLegacyChatData(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_legacy_chat_failed", "concern", concern, "path", entry.StateDBPath, "workspace_hash", entry.WorkspaceHash, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("read cursor legacy chat: %w", err)
	}
	if !found {
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor legacy chat data not found")
	}
	for _, tab := range chatData.Tabs {
		if tab.TabID != tabID || len(tab.Bubbles) == 0 {
			continue
		}
		var artifact discoveredArtifact
		artifact.Kind = discoveredKindLegacy
		artifact.Path = path
		artifact.LegacyTab = tab
		artifact.WorkspaceRoot = readWorkspaceRoot(ctx, entry)
		return artifact, nil
	}
	return emptyDiscoveredArtifact(), fmt.Errorf("cursor legacy tab %q not found", tabID)
}

func openOptionalDatabase(ctx context.Context, path string, component string) (*sql.DB, bool) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		slog.WarnContext(ctx, "providers.cursor.parser.stat_db_failed", "concern", concern, "component", component, "path", path, "err", err)
		return nil, false
	}
	db, err := cursorstore.OpenReadOnlyDatabase(ctx, path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.open_db_failed", "concern", concern, "component", component, "path", path, "err", err)
		return nil, false
	}
	return db, true
}

func backgroundComposerIDSet(ctx context.Context, db *sql.DB, path string) map[string]bool {
	backgroundIDs := make(map[string]bool)
	backgroundComposers, err := cursorstore.ListBackgroundComposers(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_background_composers_failed", "concern", concern, "path", path, "err", err)
		return backgroundIDs
	}
	for _, backgroundComposer := range backgroundComposers {
		backgroundIDs[backgroundComposer.ComposerID] = true
	}
	return backgroundIDs
}

func workspaceComposerIndex(
	ctx context.Context,
	root cursorstore.DataRoot,
) map[string]cursorstore.WorkspaceComposerInfo {
	index, err := cursorstore.BuildWorkspaceComposerIndex(ctx, root)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.workspace_composer_index_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return make(map[string]cursorstore.WorkspaceComposerInfo)
	}
	return index
}

func readWorkspaceRoot(ctx context.Context, entry cursorstore.WorkspaceEntry) string {
	workspaceRoot, err := cursorstore.ReadWorkspaceFolderPath(entry.WorkspaceJSONPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_workspace_folder_failed", "concern", concern, "path", entry.WorkspaceJSONPath, "workspace_hash", entry.WorkspaceHash, "err", err)
		return ""
	}
	return workspaceRoot
}

func globalDBPathForDiscovered(
	ctx context.Context,
	discovered discoveredArtifact,
) (string, error) {
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_data_roots_failed", "concern", concern, "path", discovered.Path, "err", err)
		return "", fmt.Errorf("resolve cursor data roots: %w", err)
	}
	if discovered.RootDir != "" {
		for _, root := range roots {
			if root.RootDir == discovered.RootDir {
				return root.GlobalDBPath, nil
			}
		}
	}
	virtualPath, err := ParseVirtualPath(discovered.Path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.virtual_path_parse_failed", "concern", concern, "path", discovered.Path, "err", err)
		return "", fmt.Errorf("parse cursor virtual path: %w", err)
	}
	for _, root := range roots {
		if RootHash(root.RootDir) == virtualPath.RootHash {
			return root.GlobalDBPath, nil
		}
	}
	slog.WarnContext(ctx, "providers.cursor.parser.root_not_found", "concern", concern, "path", discovered.Path)
	return "", fmt.Errorf("cursor root not found for %s", discovered.Path)
}

func composerArtifactKind(isBackground bool) string {
	if isBackground {
		return artifactKindCursorBackground
	}
	return artifactKindCursorComposer
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ms*int64(time.Millisecond))
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

func truncateTitle(title string) string {
	trimmed := strings.TrimSpace(title)
	runes := []rune(trimmed)
	if len(runes) <= maxTitleRunes {
		return trimmed
	}
	return string(runes[:maxTitleRunes])
}

func emptyRecord() conversation.Record {
	var record conversation.Record
	return record
}

func emptyDiscoveredArtifact() discoveredArtifact {
	var artifact discoveredArtifact
	return artifact
}
