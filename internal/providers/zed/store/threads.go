package zedstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// DataType names the stored encoding of one Zed thread payload blob.
type DataType string

const (
	// DataTypeJSON stores plain JSON bytes.
	DataTypeJSON DataType = "json"
	// DataTypeZstd stores a zstd-compressed JSON payload.
	DataTypeZstd DataType = "zstd"
)

// ThreadRow is the typed projection of one `threads` row.
type ThreadRow struct {
	ThreadID       string
	ParentThreadID string
	Summary        string
	UpdatedAt      time.Time
	CreatedAt      time.Time
	FolderPaths    []string
	DataType       DataType
	Data           []byte
}

// ReadThreadRows reads typed Zed thread rows from one threads database.
func ReadThreadRows(ctx context.Context, db *sql.DB) ([]ThreadRow, error) {
	exists, err := TableExists(ctx, db, "threads")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	rows, err := db.QueryContext(
		ctx,
		`SELECT id, parent_id, folder_paths, folder_paths_order, summary, updated_at, created_at, data_type, data
		 FROM threads
		 ORDER BY TRIM(updated_at) DESC, TRIM(created_at) DESC`,
	)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.store.threads_query_failed", "concern", "providers.zed.store", "err", err)
		return nil, fmt.Errorf("query threads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	threads := make([]ThreadRow, 0)
	for rows.Next() {
		var threadID string
		var parentThreadID sql.NullString
		var folderPaths sql.NullString
		var folderPathsOrder sql.NullString
		var summary string
		var updatedAt string
		var createdAt string
		var dataType string
		var data []byte
		if err := rows.Scan(&threadID, &parentThreadID, &folderPaths, &folderPathsOrder, &summary, &updatedAt, &createdAt, &dataType, &data); err != nil {
			return nil, fmt.Errorf("scan threads row: %w", err)
		}
		updatedTime, err := time.Parse(time.RFC3339, strings.TrimSpace(updatedAt))
		if err != nil {
			return nil, fmt.Errorf("parse threads updated_at %q: %w", updatedAt, err)
		}
		createdTime, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAt))
		if err != nil {
			return nil, fmt.Errorf("parse threads created_at %q: %w", createdAt, err)
		}
		typedDataType, err := parseDataType(dataType, threadID)
		if err != nil {
			return nil, err
		}
		threads = append(threads, ThreadRow{
			ThreadID:       threadID,
			ParentThreadID: parentThreadID.String,
			Summary:        summary,
			UpdatedAt:      updatedTime,
			CreatedAt:      createdTime,
			FolderPaths:    deserializePathList(folderPaths.String, folderPathsOrder.String),
			DataType:       typedDataType,
			Data:           append([]byte(nil), data...),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate threads rows: %w", err)
	}
	return threads, nil
}

func parseDataType(raw string, threadID string) (DataType, error) {
	switch typed := DataType(strings.TrimSpace(raw)); typed {
	case DataTypeJSON, DataTypeZstd:
		return typed, nil
	default:
		return DataType(""), fmt.Errorf("parse threads data_type %q for thread %q", raw, threadID)
	}
}
