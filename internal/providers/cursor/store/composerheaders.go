package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

// composerHeadersTable names Cursor's global composer metadata table, which
// holds a row for every composer on the machine. Each workspace database also
// keeps a registry of the composers opened in it, and both are live at once.
const composerHeadersTable = "composerHeaders"

// ComposerMetadata is the descriptive metadata Cursor keeps beside a composer:
// which project it ran against, what it is called, and whether it is archived.
// The messages themselves live elsewhere, so a composer with no metadata is
// still a readable conversation.
type ComposerMetadata struct {
	ComposerID    string
	WorkspaceRoot string
	Name          string
	Subtitle      string
	Archived      bool
	// Subagent reports that Cursor ran this chat as an agent another conversation
	// dispatched rather than one the person opened. Cursor states this outright
	// here, where a transcript on disk carries no such marker and has to be
	// classified by where it sits.
	Subagent      bool
	CreatedAt     int64
	LastUpdatedAt int64
}

// ComposerMetadataIndex is composer metadata by composer id, together with what
// went wrong reading it. A caller that caches what it builds needs both: an index
// that is merely partial looks exactly like one describing chats that genuinely
// have no workspace and no name, so Err is what tells those apart. Whatever the
// readable stores supplied is still in ByComposerID either way.
type ComposerMetadataIndex struct {
	ByComposerID map[string]ComposerMetadata
	Err          error
}

// composerHeadersValue models the consumed subset of Cursor's undocumented,
// version-pinned composerHeaders value payload. Unconsumed fields are
// intentionally ignored.
type composerHeadersValue struct {
	Name     string `json:"name"`
	Subtitle string `json:"subtitle"`
	// IsArchived is a pointer so that a payload saying nothing about archiving is
	// told apart from one saying the chat is not archived. Only the second is an
	// answer, and the row's own column still answers for the first.
	IsArchived          *bool                       `json:"isArchived"`
	WorkspaceIdentifier composerWorkspaceIdentifier `json:"workspaceIdentifier"`
}

// composerWorkspaceIdentifier models the workspace a composer ran against.
// Cursor writes one of two shapes and never both: a `uri` when the window opened
// a single folder, and a `configPath` when it opened a multi-root workspace,
// where the path names the workspace file rather than a directory. Measured on a
// real store, 825 composers carry a uri and 1,323 carry a configPath, so reading
// only the first shape leaves the majority with no workspace at all.
type composerWorkspaceIdentifier struct {
	URI        composerWorkspaceURI `json:"uri"`
	ConfigPath composerWorkspaceURI `json:"configPath"`
}

// composerWorkspaceURI models Cursor's serialized workspace URI. Cursor writes
// the same location twice: fsPath in the local platform's form and path in
// slash form. Either one can be absent.
type composerWorkspaceURI struct {
	FsPath string `json:"fsPath"`
	Path   string `json:"path"`
}

func decodeComposerHeadersValueJSON(data []byte) (composerHeadersValue, error) {
	var value composerHeadersValue
	if err := json.Unmarshal(data, &value); err != nil {
		return composerHeadersValue{}, CursorJSONDecodeError{Description: "composer headers value", Err: err}
	}
	return value, nil
}

// ReadComposerMetadataIndex indexes Cursor's global composerHeaders table by
// composer id. It returns an empty index when the table is absent, which is how
// a Cursor build that predates the table presents; that build keeps composer
// metadata only in each workspace database, which [BuildComposerMetadataIndex]
// reads alongside this one.
func ReadComposerMetadataIndex(ctx context.Context, db *sql.DB) (map[string]ComposerMetadata, error) {
	if db == nil {
		return nil, fmt.Errorf("read cursor composer metadata: nil database")
	}
	index := make(map[string]ComposerMetadata)

	exists, err := TableExists(ctx, db, composerHeadersTable)
	if err != nil {
		return nil, err
	}
	if !exists {
		return index, nil
	}

	// The numeric columns are cast in SQL rather than trusted to arrive as
	// numbers. SQLite stores whatever it was given, so one row holding a timestamp
	// as text would otherwise fail to scan and cost every composer in the table its
	// metadata. A cast turns that row into a zero timestamp, which is what the row
	// is worth, and leaves the rest readable.
	rows, err := db.QueryContext(ctx, "SELECT composerId, CAST(createdAt AS INTEGER), CAST(lastUpdatedAt AS INTEGER), CAST(isArchived AS INTEGER), CAST(isSubagent AS INTEGER), value FROM composerHeaders")
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_headers_query_failed", "concern", concern, "table", composerHeadersTable, "err", err)
		return nil, fmt.Errorf("query cursor %s rows: %w", composerHeadersTable, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		metadata, ok, scanErr := scanComposerMetadataRow(ctx, rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if !ok {
			continue
		}
		index[metadata.ComposerID] = metadata
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_headers_iterate_failed", "concern", concern, "table", composerHeadersTable, "err", err)
		return nil, fmt.Errorf("iterate cursor %s rows: %w", composerHeadersTable, err)
	}
	return index, nil
}

// scanComposerMetadataRow reads one composerHeaders row. A row whose value does
// not decode still yields its typed columns, because the timestamps and the
// archived flag are what a listing needs most and neither lives in the JSON.
func scanComposerMetadataRow(ctx context.Context, rows *sql.Rows) (ComposerMetadata, bool, error) {
	// composerId is read as nullable so that one row naming no composer is
	// skipped rather than failing the whole read. Such a row describes nothing
	// Clyde can attach metadata to, and letting it abort the scan would hold every
	// Cursor conversation out of the index for as long as the row sat there.
	var composerIDColumn sql.NullString
	var createdAt sql.NullInt64
	var lastUpdatedAt sql.NullInt64
	var isArchived sql.NullInt64
	var isSubagent sql.NullInt64
	var value []byte

	if err := rows.Scan(&composerIDColumn, &createdAt, &lastUpdatedAt, &isArchived, &isSubagent, &value); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_headers_scan_failed", "concern", concern, "table", composerHeadersTable, "err", err)
		return emptyComposerMetadata(), false, fmt.Errorf("scan cursor %s row: %w", composerHeadersTable, err)
	}
	composerID := strings.TrimSpace(composerIDColumn.String)
	if composerID == "" {
		return emptyComposerMetadata(), false, nil
	}

	metadata := ComposerMetadata{
		ComposerID:    composerID,
		WorkspaceRoot: "",
		Name:          "",
		Subtitle:      "",
		Archived:      isArchived.Valid && isArchived.Int64 != 0,
		Subagent:      isSubagent.Valid && isSubagent.Int64 != 0,
		CreatedAt:     createdAt.Int64,
		LastUpdatedAt: lastUpdatedAt.Int64,
	}

	decoded, err := decodeComposerHeadersValueJSON(value)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_headers_decode_failed", "concern", concern, "composer_id", composerID, "err", err)
		return metadata, true, nil
	}
	metadata.Name = decoded.Name
	metadata.Subtitle = decoded.Subtitle
	metadata.WorkspaceRoot = workspaceRootFromIdentifier(decoded.WorkspaceIdentifier)
	// A decoded flag replaces the column rather than being combined with it.
	// Archiving is reversible, so a payload saying a chat is not archived is an
	// answer, and folding the two together would leave a chat the person
	// unarchived hidden for as long as the older of the two lagged. A payload that
	// omits the field says nothing, and the column keeps its answer.
	if decoded.IsArchived != nil {
		metadata.Archived = *decoded.IsArchived
	}
	return metadata, true, nil
}

func emptyComposerMetadata() ComposerMetadata {
	var metadata ComposerMetadata
	return metadata
}

// workspaceRootFromIdentifier reads the workspace a composer ran against,
// preferring the single folder a window opened and falling back to the workspace
// file a multi-root window opened.
//
// A workspace file is a location rather than a directory, which is the same thing
// Clyde already records for a modern transcript: those name their project by the
// `.code-workspace` path too, only derived lossily from a directory name. Taking
// the path Cursor wrote is the exact form of the identity Clyde already uses.
func workspaceRootFromIdentifier(identifier composerWorkspaceIdentifier) string {
	folder := workspaceRootFromURI(identifier.URI)
	if folder != "" {
		return folder
	}
	return workspaceRootFromURI(identifier.ConfigPath)
}

// workspaceRootFromURI reads a location from Cursor's serialized URI, preferring
// the platform-form path Cursor already resolved and falling back to the slash
// form when it is absent.
func workspaceRootFromURI(uri composerWorkspaceURI) string {
	fsPath := strings.TrimSpace(uri.FsPath)
	if fsPath != "" {
		return filepath.FromSlash(fsPath)
	}
	slashPath := strings.TrimSpace(uri.Path)
	if slashPath == "" {
		return ""
	}
	return filepath.FromSlash(slashPath)
}
