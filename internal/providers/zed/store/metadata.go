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
		var archived bool
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
		updatedTime, err := time.Parse(time.RFC3339, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse sidebar_threads updated_at %q: %w", updatedAt, err)
		}
		createdTime, err := parseOptionalRFC3339(ctx, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse sidebar_threads created_at: %w", err)
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
			Archived:          archived,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sidebar_threads rows: %w", err)
	}
	return metadata, nil
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
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value.String)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.store.optional_time_parse_failed", "concern", "providers.zed.store", "value", value.String, "err", err)
		return time.Time{}, fmt.Errorf("parse optional RFC3339 %q: %w", value.String, err)
	}
	return parsed, nil
}
