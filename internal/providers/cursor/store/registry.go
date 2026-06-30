package cursorstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

const allComposersKey = "composer.composerData.allComposers"

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
	entries, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.workspace_entries_list_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return nil, fmt.Errorf("list cursor workspace entries in %s: %w", root.WorkspaceStorageDir, err)
	}

	index := make(map[string]WorkspaceComposerInfo)
	for _, entry := range entries {
		addWorkspaceComposers(ctx, index, entry)
	}
	return index, nil
}

func addWorkspaceComposers(ctx context.Context, index map[string]WorkspaceComposerInfo, entry WorkspaceEntry) {
	db, err := OpenReadOnlyDatabase(ctx, entry.StateDBPath)
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()

	value, found, err := ReadKVValue(ctx, db, KVTableItemTable, allComposersKey)
	if err != nil {
		return
	}
	if !found {
		return
	}

	document, err := DecodeAllComposersJSON(value)
	if err != nil {
		return
	}

	cwd, err := ReadWorkspaceFolderPath(entry.WorkspaceJSONPath)
	if err != nil {
		return
	}

	for _, composer := range document.AllComposers {
		index[composer.ComposerID] = WorkspaceComposerInfo{
			ComposerID:    composer.ComposerID,
			Cwd:           cwd,
			Name:          composer.Name,
			Subtitle:      composer.Subtitle,
			Archived:      composer.IsArchived,
			CreatedAt:     composer.CreatedAt,
			LastUpdatedAt: composer.LastUpdatedAt,
		}
	}
}
