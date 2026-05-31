package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm/capture"
)

// openMITMCaptureStore maps the typed capture-store config into the capture
// package's Config and opens the shared SQLite sink on the daemon-lifetime
// context. DBPath is defaulted at config load; the remaining fields default
// inside the capture package when zero. The age and interval fields convert
// from the config Duration wrapper.
func openMITMCaptureStore(ctx context.Context, cfg *config.Config, log *slog.Logger) (*capture.Store, error) {
	store, err := capture.Open(ctx, capture.Config{
		DBPath:            cfg.MITM.CaptureStore.DBPath,
		MaxBodyBytes:      cfg.MITM.CaptureStore.MaxBodyBytes,
		RetentionMaxAge:   cfg.MITM.CaptureStore.RetentionMaxAge.AsDuration(),
		RetentionMaxBytes: cfg.MITM.CaptureStore.RetentionMaxBytes,
		RetentionInterval: cfg.MITM.CaptureStore.RetentionInterval.AsDuration(),
		QueueDepth:        0,
	}, log)
	if err != nil {
		log.ErrorContext(ctx, "daemon.mitm.capture_store_open_failed", "db_path", cfg.MITM.CaptureStore.DBPath, "err", err)
		return nil, fmt.Errorf("open mitm capture store %s: %w", cfg.MITM.CaptureStore.DBPath, err)
	}
	return store, nil
}
