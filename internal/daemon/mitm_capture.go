package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm/capture"
)

// openMITMCaptureStore maps the typed capture-store config into the capture
// package's Config and opens the shared SQLite sink on the daemon-lifetime
// context. DBPath is defaulted at config load; the remaining fields default
// inside the capture package when zero. The age and interval fields convert
// from the config Duration wrapper.
func openMITMCaptureStore(ctx context.Context, cfg *config.Config, log *slog.Logger) (*capture.Store, error) {
	dbPath := cfg.MITM.CaptureStore.DBPath
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		log.ErrorContext(ctx, "daemon.capture_store_directory_failed", "db_path", dbPath, "err", err)
		return nil, fmt.Errorf("create capture store directory for %s: %w", dbPath, err)
	}
	store, err := capture.Open(ctx, capture.Config{
		DBPath:            dbPath,
		MaxBodyBytes:      cfg.MITM.CaptureStore.MaxBodyBytes,
		RetentionMaxAge:   cfg.MITM.CaptureStore.RetentionMaxAge.AsDuration(),
		RetentionMaxBytes: cfg.MITM.CaptureStore.RetentionMaxBytes,
		RetentionInterval: cfg.MITM.CaptureStore.RetentionInterval.AsDuration(),
		QueueDepth:        0,
	}, log)
	if err != nil {
		log.ErrorContext(ctx, "daemon.capture_store_open_failed", "db_path", dbPath, "err", err)
		return nil, fmt.Errorf("open capture store %s: %w", dbPath, err)
	}
	return store, nil
}
