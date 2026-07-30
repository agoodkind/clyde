package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
)

// Cursor keeps a workspace's composer registry under one of two keys, holding
// the same document either way. Current builds write workspaceComposerDataKey;
// older ones wrote legacyAllComposersKey. Both are read in that order, because a
// machine that has run both builds still carries workspaces written by the older
// one.
const (
	workspaceComposerDataKey = "composer.composerData"
	legacyAllComposersKey    = "composer.composerData.allComposers"
)

// AllComposersDocument models the consumed subset of Cursor's undocumented,
// version-pinned workspace composer registry. Unconsumed fields are
// intentionally ignored.
type AllComposersDocument struct {
	AllComposers       []AllComposersEntry `json:"allComposers"`
	SelectedComposerID string              `json:"selectedComposerId"`
}

// AllComposersEntry models one consumed composer entry in Cursor's workspace
// registry.
type AllComposersEntry struct {
	ComposerID    string `json:"composerId"`
	Name          string `json:"name"`
	CreatedAt     int64  `json:"createdAt"`
	LastUpdatedAt int64  `json:"lastUpdatedAt"`
	Subtitle      string `json:"subtitle"`
	IsArchived    bool   `json:"isArchived"`
}

// WorkspaceComposerInfo joins one Cursor composer id to the workspace metadata
// Clyde consumes.
type WorkspaceComposerInfo struct {
	ComposerID    string
	Cwd           string
	Name          string
	Subtitle      string
	Archived      bool
	CreatedAt     int64
	LastUpdatedAt int64
}

// DecodeAllComposersJSON decodes one Cursor workspace composer registry value.
func DecodeAllComposersJSON(data []byte) (AllComposersDocument, error) {
	var document AllComposersDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return AllComposersDocument{}, CursorJSONDecodeError{Description: "all composers", Err: err}
	}
	return document, nil
}

// BuildWorkspaceComposerIndex indexes Cursor workspace composer registry rows
// by composer id. Individual unreadable workspace databases or descriptors are
// skipped so one bad workspace does not prevent indexing the rest of the root.
func BuildWorkspaceComposerIndex(ctx context.Context, root DataRoot) (map[string]WorkspaceComposerInfo, error) {
	listing, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.workspace_entries_list_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return nil, fmt.Errorf("list cursor workspace entries in %s: %w", root.WorkspaceStorageDir, err)
	}

	index := make(map[string]WorkspaceComposerInfo)
	for _, entry := range listing.Entries {
		addWorkspaceComposers(ctx, index, entry)
	}
	return index, nil
}

// BuildComposerMetadataIndex indexes every composer Cursor knows about by
// composer id, reading whichever store this Cursor build keeps that metadata in.
//
// Cursor writes composer metadata in two places at once, not one after the
// other. Each workspace database holds a registry of the composers opened in it,
// and the global composerHeaders table holds a row for every composer on the
// machine. Both are live on a current build, so both are read, and the global
// table takes precedence field by field where the two describe the same composer.
//
// Nothing here decides whether a composer is a conversation, so a composer that
// appears in neither store is still indexed from its own record, only without a
// workspace root or a name.
//
// A store that cannot be read is reported through Err while what the other store
// supplied is still returned. The two are separate answers because the caller
// caches what it builds and will not rebuild it while the conversation is
// unchanged, so metadata that is merely partial must not be mistaken for the
// whole of it, and a chat the readable store described should not lose that
// description because its neighbour failed.
func BuildComposerMetadataIndex(
	ctx context.Context,
	globalDB *sql.DB,
	root DataRoot,
) ComposerMetadataIndex {
	index := make(map[string]ComposerMetadata)

	workspaceIndex, workspaceErr := BuildWorkspaceComposerIndex(ctx, root)
	if workspaceErr != nil {
		slog.WarnContext(ctx, "providers.cursor.store.workspace_composer_index_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", workspaceErr)
	}
	for composerID, info := range workspaceIndex {
		index[composerID] = ComposerMetadata{
			ComposerID:    composerID,
			WorkspaceRoot: info.Cwd,
			Name:          info.Name,
			Subtitle:      info.Subtitle,
			Archived:      info.Archived,
			// The workspace registry predates the subagent flag and records no
			// equivalent, so a composer known only here is left unmarked rather than
			// called the person's own.
			Subagent:      false,
			CreatedAt:     info.CreatedAt,
			LastUpdatedAt: info.LastUpdatedAt,
		}
	}

	globalIndex, globalErr := ReadComposerMetadataIndex(ctx, globalDB)
	if globalErr != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_metadata_index_failed", "concern", concern, "path", root.GlobalDBPath, "err", globalErr)
		return ComposerMetadataIndex{ByComposerID: index, Err: globalErr}
	}
	for composerID, global := range globalIndex {
		workspace, known := index[composerID]
		if !known {
			index[composerID] = global
			continue
		}
		index[composerID] = mergeComposerMetadata(workspace, global)
	}
	return ComposerMetadataIndex{ByComposerID: index, Err: workspaceErr}
}

// mergeComposerMetadata lets the global store win field by field rather than
// wholesale. A field the global row could not supply is absent, not empty, and a
// composer whose global JSON failed to decode still has a name and a workspace
// root in the older per-workspace registry. Overwriting those with blanks would
// lose metadata Clyde can still read.
func mergeComposerMetadata(workspace ComposerMetadata, global ComposerMetadata) ComposerMetadata {
	merged := global
	if merged.WorkspaceRoot == "" {
		merged.WorkspaceRoot = workspace.WorkspaceRoot
	}
	if merged.Name == "" {
		merged.Name = workspace.Name
	}
	if merged.Subtitle == "" {
		merged.Subtitle = workspace.Subtitle
	}
	if merged.CreatedAt == 0 {
		merged.CreatedAt = workspace.CreatedAt
	}
	if merged.LastUpdatedAt == 0 {
		merged.LastUpdatedAt = workspace.LastUpdatedAt
	}
	// Archived is deliberately not filled in from the workspace registry. It is
	// the one field a person reverses, and the global row is the store Cursor
	// keeps current, so a workspace registry that has not caught up would
	// otherwise hold an unarchived chat hidden from every listing and every
	// search. A composer only the workspace registry knows about still carries its
	// own flag, because no global row exists there to answer instead.
	return merged
}

// readWorkspaceComposerRegistry reads one workspace's composer registry from
// whichever key this Cursor build wrote it under. A key that is absent is not a
// failure, so the search moves on to the next one and reports only whether a
// registry was found at all.
func readWorkspaceComposerRegistry(ctx context.Context, db *sql.DB) (AllComposersDocument, bool) {
	for _, key := range []string{workspaceComposerDataKey, legacyAllComposersKey} {
		value, found, err := ReadKVValue(ctx, db, KVTableItemTable, key)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.workspace_registry_read_failed", "concern", concern, "key", key, "err", err)
			continue
		}
		if !found {
			continue
		}
		document, err := DecodeAllComposersJSON(value)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.workspace_registry_decode_failed", "concern", concern, "key", key, "err", err)
			continue
		}
		if len(document.AllComposers) == 0 {
			continue
		}
		return document, true
	}
	var emptyDocument AllComposersDocument
	return emptyDocument, false
}

func addWorkspaceComposers(ctx context.Context, index map[string]WorkspaceComposerInfo, entry WorkspaceEntry) {
	db, err := OpenReadOnlyDatabase(ctx, entry.StateDBPath)
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()

	document, found := readWorkspaceComposerRegistry(ctx, db)
	if !found {
		return
	}

	// Neither a missing folder descriptor nor an unreadable one costs this
	// workspace's composers anything but their folder path. A workspace with no
	// descriptor is a window opened on no folder, so it has no working directory
	// rather than an unreadable one, and neither case is a reason to drop the
	// chats: their names, subtitles, archived flags, and timestamps are already in
	// the registry, so dropping them would leave them untitled for a reason that
	// has nothing to do with them.
	cwd := ""
	if entry.WorkspaceJSONPath != "" {
		folderPath, folderErr := ReadWorkspaceFolderPath(entry.WorkspaceJSONPath)
		if folderErr != nil {
			slog.WarnContext(ctx, "providers.cursor.store.workspace_folder_read_failed", "concern", concern, "path", entry.WorkspaceJSONPath, "workspace_hash", entry.WorkspaceHash, "err", folderErr)
		} else {
			cwd = folderPath
		}
	}

	for _, composer := range document.AllComposers {
		// The index spans every workspace and is keyed by composer id, and a chat
		// opened in more than one workspace appears in each registry. A path this
		// workspace could not read must not replace one another workspace already
		// supplied for the same chat, so an empty path never overwrites a real one.
		composerCwd := cwd
		if composerCwd == "" {
			composerCwd = index[composer.ComposerID].Cwd
		}
		index[composer.ComposerID] = WorkspaceComposerInfo{
			ComposerID:    composer.ComposerID,
			Cwd:           composerCwd,
			Name:          composer.Name,
			Subtitle:      composer.Subtitle,
			Archived:      composer.IsArchived,
			CreatedAt:     composer.CreatedAt,
			LastUpdatedAt: composer.LastUpdatedAt,
		}
	}
}
