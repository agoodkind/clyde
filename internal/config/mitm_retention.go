package config

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func readCaptureRetentionPresence(data []byte, store *MITMCaptureStoreConfig) error {
	var raw struct {
		MITM struct {
			CaptureStore struct {
				Age      *Duration `toml:"retention_max_age"`
				Bytes    *int64    `toml:"retention_max_bytes"`
				Interval *Duration `toml:"retention_interval"`
			} `toml:"capture_store"`
		} `toml:"mitm"`
	}
	if err := toml.Unmarshal(data, &raw); err != nil {
		slog.Warn("config.capture_retention_parse_failed", "err", err)
		return fmt.Errorf("read capture retention configuration: %w", err)
	}
	store.retentionAgePresent = raw.MITM.CaptureStore.Age != nil
	store.retentionBytesPresent = raw.MITM.CaptureStore.Bytes != nil
	store.retentionIntervalPresent = raw.MITM.CaptureStore.Interval != nil
	return nil
}

func applyCaptureRetentionDefaults(store *MITMCaptureStoreConfig) error {
	if !store.retentionResolved && !store.retentionAgePresent && store.RetentionMaxAge == 0 {
		store.RetentionMaxAge = Duration(168 * time.Hour)
	}
	if !store.retentionResolved && !store.retentionBytesPresent && store.RetentionMaxBytes == 0 {
		store.RetentionMaxBytes = 5_000_000_000
	}
	if !store.retentionResolved && !store.retentionIntervalPresent && store.RetentionInterval == 0 {
		store.RetentionInterval = Duration(time.Second)
	}
	if store.RetentionMaxAge < 0 {
		return fmt.Errorf("mitm.capture_store.retention_max_age must be non-negative")
	}
	if store.RetentionMaxBytes < 0 {
		return fmt.Errorf("mitm.capture_store.retention_max_bytes must be non-negative")
	}
	if store.RetentionInterval <= 0 {
		return fmt.Errorf("mitm.capture_store.retention_interval must be positive")
	}
	if store.RetentionMaxAge == 0 && store.RetentionMaxBytes == 0 {
		return fmt.Errorf("mitm.capture_store must enable age or size retention")
	}
	store.retentionResolved = true
	return nil
}

func applyMITMCaptureStoreDefaults(store *MITMCaptureStoreConfig, captureDir string) {
	store.DBPath = strings.TrimSpace(store.DBPath)
	if store.DBPath == "" {
		store.DBPath = filepath.Join(captureDir, "capture.db")
		return
	}
	store.DBPath = cleanExpandedPath(store.DBPath)
}
