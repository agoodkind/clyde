package zedstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SidebarThreadMetadata is the typed projection of one `sidebar_threads` row.
type SidebarThreadMetadata struct {
	SessionID         string
	AgentID           string
	Title             string
	TitleOverride     string
	UpdatedAt         time.Time
	CreatedAt         time.Time
	FolderPaths       []string
	MainWorktreePaths []string
	Archived          bool
}

// SidebarTerminalThreadMetadata is the typed projection of one
// `sidebar_terminal_threads` row.
type SidebarTerminalThreadMetadata struct {
	TerminalID        string
	Title             string
	CustomTitle       string
	CreatedAt         time.Time
	WorkingDirectory  string
	FolderPaths       []string
	MainWorktreePaths []string
	RemoteConnection  string
}

// ReadSidebarThreads reads typed Zed sidebar thread metadata rows from one
// metadata database.
func ReadSidebarThreads(ctx context.Context, db *sql.DB) ([]SidebarThreadMetadata, error) {
	exists, err := TableExists(ctx, db, "sidebar_threads")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	rows, err := db.QueryContext(
		ctx,
		`SELECT session_id, agent_id, title, title_override, updated_at, created_at,
		 folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order
		 FROM sidebar_threads
		 WHERE session_id IS NOT NULL
		 ORDER BY updated_at DESC`,
	)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.store.sidebar_query_failed", "concern", "providers.zed.store", "err", err)
		return nil, fmt.Errorf("query sidebar_threads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	metadata := make([]SidebarThreadMetadata, 0)
	for rows.Next() {
		var sessionID string
		var agentID sql.NullString
		var title string
		var titleOverride sql.NullString
		var updatedAt string
		var createdAt sql.NullString
		var folderPaths sql.NullString
		var folderPathsOrder sql.NullString
		var archived sql.NullInt64
		var mainWorktreePaths sql.NullString
		var mainWorktreePathsOrder sql.NullString
		if err := rows.Scan(
			&sessionID,
			&agentID,
			&title,
			&titleOverride,
			&updatedAt,
			&createdAt,
			&folderPaths,
			&folderPathsOrder,
			&archived,
			&mainWorktreePaths,
			&mainWorktreePathsOrder,
		); err != nil {
			return nil, fmt.Errorf("scan sidebar_threads row: %w", err)
		}
		isArchived := archived.Valid && archived.Int64 != 0
		updatedTime, err := time.Parse(time.RFC3339, strings.TrimSpace(updatedAt))
		if err != nil {
			return nil, fmt.Errorf("parse sidebar_threads updated_at %q: %w", updatedAt, err)
		}
		createdTime, err := parseOptionalRFC3339(ctx, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse sidebar_threads created_at %q: %w", createdAt.String, err)
		}
		metadata = append(metadata, SidebarThreadMetadata{
			SessionID:         sessionID,
			AgentID:           agentID.String,
			Title:             title,
			TitleOverride:     titleOverride.String,
			UpdatedAt:         updatedTime,
			CreatedAt:         createdTime,
			FolderPaths:       deserializePathList(folderPaths.String, folderPathsOrder.String),
			MainWorktreePaths: deserializePathList(mainWorktreePaths.String, mainWorktreePathsOrder.String),
			Archived:          isArchived,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sidebar_threads rows: %w", err)
	}
	return metadata, nil
}

// ReadSidebarThreadBySession reads one typed Zed sidebar thread metadata row.
func ReadSidebarThreadBySession(ctx context.Context, db *sql.DB, sessionID string) (SidebarThreadMetadata, bool, error) {
	var emptyMetadata SidebarThreadMetadata

	exists, err := TableExists(ctx, db, "sidebar_threads")
	if err != nil {
		return emptyMetadata, false, err
	}
	if !exists {
		return emptyMetadata, false, nil
	}

	row := db.QueryRowContext(
		ctx,
		`SELECT session_id, agent_id, title, title_override, updated_at, created_at,
		 folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order
		 FROM sidebar_threads
		 WHERE session_id = ?
		 ORDER BY updated_at DESC
		 LIMIT 1`,
		sessionID,
	)

	var rowSessionID string
	var agentID sql.NullString
	var title string
	var titleOverride sql.NullString
	var updatedAt string
	var createdAt sql.NullString
	var folderPaths sql.NullString
	var folderPathsOrder sql.NullString
	var archived sql.NullInt64
	var mainWorktreePaths sql.NullString
	var mainWorktreePathsOrder sql.NullString
	if err := row.Scan(
		&rowSessionID,
		&agentID,
		&title,
		&titleOverride,
		&updatedAt,
		&createdAt,
		&folderPaths,
		&folderPathsOrder,
		&archived,
		&mainWorktreePaths,
		&mainWorktreePathsOrder,
	); err != nil {
		if err == sql.ErrNoRows {
			return emptyMetadata, false, nil
		}
		slog.WarnContext(ctx, "providers.zed.store.sidebar_row_by_session_scan_failed", "concern", "providers.zed.store", "session_id", sessionID, "err", err)
		return emptyMetadata, false, fmt.Errorf("scan sidebar_threads row by session: %w", err)
	}

	updatedTime, err := time.Parse(time.RFC3339, strings.TrimSpace(updatedAt))
	if err != nil {
		return emptyMetadata, false, fmt.Errorf("parse sidebar_threads updated_at %q: %w", updatedAt, err)
	}
	createdTime, err := parseOptionalRFC3339(ctx, createdAt)
	if err != nil {
		return emptyMetadata, false, fmt.Errorf("parse sidebar_threads created_at %q: %w", createdAt.String, err)
	}
	isArchived := archived.Valid && archived.Int64 != 0

	return SidebarThreadMetadata{
		SessionID:         rowSessionID,
		AgentID:           agentID.String,
		Title:             title,
		TitleOverride:     titleOverride.String,
		UpdatedAt:         updatedTime,
		CreatedAt:         createdTime,
		FolderPaths:       deserializePathList(folderPaths.String, folderPathsOrder.String),
		MainWorktreePaths: deserializePathList(mainWorktreePaths.String, mainWorktreePathsOrder.String),
		Archived:          isArchived,
	}, true, nil
}

// ReadSidebarTerminalThreads reads typed terminal metadata rows from one Zed
// metadata database.
func ReadSidebarTerminalThreads(ctx context.Context, db *sql.DB) ([]SidebarTerminalThreadMetadata, error) {
	exists, err := TableExists(ctx, db, "sidebar_terminal_threads")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	rows, err := db.QueryContext(
		ctx,
		`SELECT terminal_id, title, custom_title, created_at, working_directory,
		 folder_paths, folder_paths_order, main_worktree_paths, main_worktree_paths_order,
		 remote_connection
		 FROM sidebar_terminal_threads
		 WHERE terminal_id IS NOT NULL
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.store.sidebar_terminal_query_failed", "concern", "providers.zed.store", "err", err)
		return nil, fmt.Errorf("query sidebar_terminal_threads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	metadata := make([]SidebarTerminalThreadMetadata, 0)
	for rows.Next() {
		var terminalID string
		var title string
		var customTitle sql.NullString
		var createdAt string
		var workingDirectory sql.NullString
		var folderPaths sql.NullString
		var folderPathsOrder sql.NullString
		var mainWorktreePaths sql.NullString
		var mainWorktreePathsOrder sql.NullString
		var remoteConnection sql.NullString
		if err := rows.Scan(
			&terminalID,
			&title,
			&customTitle,
			&createdAt,
			&workingDirectory,
			&folderPaths,
			&folderPathsOrder,
			&mainWorktreePaths,
			&mainWorktreePathsOrder,
			&remoteConnection,
		); err != nil {
			return nil, fmt.Errorf("scan sidebar_terminal_threads row: %w", err)
		}
		createdTime, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAt))
		if err != nil {
			return nil, fmt.Errorf("parse sidebar_terminal_threads created_at %q: %w", createdAt, err)
		}
		metadata = append(metadata, SidebarTerminalThreadMetadata{
			TerminalID:        terminalID,
			Title:             title,
			CustomTitle:       customTitle.String,
			CreatedAt:         createdTime,
			WorkingDirectory:  workingDirectory.String,
			FolderPaths:       deserializePathList(folderPaths.String, folderPathsOrder.String),
			MainWorktreePaths: deserializePathList(mainWorktreePaths.String, mainWorktreePathsOrder.String),
			RemoteConnection:  remoteConnection.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sidebar_terminal_threads rows: %w", err)
	}
	return metadata, nil
}

// ReadSidebarTerminalByID reads one typed terminal metadata row from a Zed
// metadata database.
func ReadSidebarTerminalByID(ctx context.Context, db *sql.DB, terminalID string) (SidebarTerminalThreadMetadata, bool, error) {
	var emptyMetadata SidebarTerminalThreadMetadata

	exists, err := TableExists(ctx, db, "sidebar_terminal_threads")
	if err != nil {
		return emptyMetadata, false, err
	}
	if !exists {
		return emptyMetadata, false, nil
	}

	row := db.QueryRowContext(
		ctx,
		`SELECT terminal_id, title, custom_title, created_at, working_directory,
		 folder_paths, folder_paths_order, main_worktree_paths, main_worktree_paths_order,
		 remote_connection
		 FROM sidebar_terminal_threads
		 WHERE terminal_id = ?
		 LIMIT 1`,
		terminalID,
	)

	var rowTerminalID string
	var title string
	var customTitle sql.NullString
	var createdAt string
	var workingDirectory sql.NullString
	var folderPaths sql.NullString
	var folderPathsOrder sql.NullString
	var mainWorktreePaths sql.NullString
	var mainWorktreePathsOrder sql.NullString
	var remoteConnection sql.NullString
	if err := row.Scan(
		&rowTerminalID,
		&title,
		&customTitle,
		&createdAt,
		&workingDirectory,
		&folderPaths,
		&folderPathsOrder,
		&mainWorktreePaths,
		&mainWorktreePathsOrder,
		&remoteConnection,
	); err != nil {
		if err == sql.ErrNoRows {
			return emptyMetadata, false, nil
		}
		slog.WarnContext(ctx, "providers.zed.store.sidebar_terminal_row_scan_failed", "concern", "providers.zed.store", "terminal_id", terminalID, "err", err)
		return emptyMetadata, false, fmt.Errorf("scan sidebar_terminal_threads row by terminal id: %w", err)
	}

	createdTime, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAt))
	if err != nil {
		return emptyMetadata, false, fmt.Errorf("parse sidebar_terminal_threads created_at %q: %w", createdAt, err)
	}
	return SidebarTerminalThreadMetadata{
		TerminalID:        rowTerminalID,
		Title:             title,
		CustomTitle:       customTitle.String,
		CreatedAt:         createdTime,
		WorkingDirectory:  workingDirectory.String,
		FolderPaths:       deserializePathList(folderPaths.String, folderPathsOrder.String),
		MainWorktreePaths: deserializePathList(mainWorktreePaths.String, mainWorktreePathsOrder.String),
		RemoteConnection:  remoteConnection.String,
	}, true, nil
}

func deserializePathList(pathsText, orderText string) []string {
	trimmedPaths := strings.TrimSpace(pathsText)
	if trimmedPaths == "" {
		return nil
	}
	paths := strings.Split(trimmedPaths, "\n")
	if strings.TrimSpace(orderText) == "" {
		return paths
	}

	orderParts := strings.Split(strings.TrimSpace(orderText), ",")
	if len(orderParts) != len(paths) {
		return paths
	}
	type orderedPath struct {
		Order int
		Path  string
	}
	ordered := make([]orderedPath, 0, len(paths))
	for i, path := range paths {
		index, err := strconv.Atoi(strings.TrimSpace(orderParts[i]))
		if err != nil {
			return paths
		}
		ordered = append(ordered, orderedPath{Order: index, Path: path})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Order < ordered[j].Order
	})
	out := make([]string, 0, len(ordered))
	for _, entry := range ordered {
		out = append(out, entry.Path)
	}
	return out
}

func parseOptionalRFC3339(ctx context.Context, value sql.NullString) (time.Time, error) {
	trimmed := strings.TrimSpace(value.String)
	if !value.Valid || trimmed == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.store.optional_time_parse_failed", "concern", "providers.zed.store", "value", value.String, "err", err)
		return time.Time{}, fmt.Errorf("parse optional RFC3339 %q: %w", value.String, err)
	}
	return parsed, nil
}
