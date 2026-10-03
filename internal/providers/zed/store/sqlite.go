package zedstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"

	_ "github.com/mattn/go-sqlite3" // database/sql driver "sqlite3"
)

// OpenReadOnlyDatabase opens one SQLite database in read-only immutable mode.
func OpenReadOnlyDatabase(ctx context.Context, path string) (*sql.DB, error) {
	if ctx == nil {
		return nil, fmt.Errorf("open zed sqlite database %s: nil context", path)
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "mode=ro&immutable=1&_busy_timeout=5000",
	}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.store.sqlite_open_failed", "concern", "providers.zed.store", "path", path, "err", err)
		return nil, fmt.Errorf("open zed sqlite database %s: %w", path, err)
	}
	if pingErr := db.PingContext(ctx); pingErr != nil {
		_ = db.Close()
		slog.WarnContext(ctx, "providers.zed.store.sqlite_ping_failed", "concern", "providers.zed.store", "path", path, "err", pingErr)
		return nil, fmt.Errorf("ping zed sqlite database %s: %w", path, pingErr)
	}
	return db, nil
}

// TableExists reports whether one named table exists in the SQLite database.
func TableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
		table,
	).Scan(&count)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.store.sqlite_master_query_failed", "concern", "providers.zed.store", "table", table, "err", err)
		return false, fmt.Errorf("query sqlite_master for %s: %w", table, err)
	}
	return count > 0, nil
}
