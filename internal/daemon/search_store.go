package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/clyde/internal/config"
	searchstore "goodkind.io/clyde/internal/search/store"
)

// openSearchStore opens the SQLite search job store under the state dir,
// creating the parent directory if needed. The store backs async conversation
// search so job state and results survive a daemon reload.
func openSearchStore(ctx context.Context, log *slog.Logger) (*searchstore.Store, error) {
	dbPath := filepath.Join(config.DefaultStateDir(), "search", "jobs.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create search store dir: %w", err)
	}
	store, err := searchstore.Open(ctx, searchstore.Config{
		DBPath:            dbPath,
		RetentionMaxAge:   0,
		RetentionInterval: 0,
	}, log)
	if err != nil {
		log.ErrorContext(ctx, "daemon.search.store_open_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "db_path", dbPath, "err", err)
		return nil, fmt.Errorf("open search store %s: %w", dbPath, err)
	}
	return store, nil
}
