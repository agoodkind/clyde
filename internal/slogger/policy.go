package slogger

import (
	"log/slog"

	"goodkind.io/gklog"
)

// RotationPolicy is the resolved file rotation budget for one slogger-owned
// file sink. Config packages own schema defaults; slogger only converts this
// narrow input to the gklog runtime type.
type RotationPolicy struct {
	Enabled    bool
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   *bool
}

// FileSinkPolicy describes the process-wide JSONL sink that SetupWithPolicy
// should install.
type FileSinkPolicy struct {
	Enabled  bool
	Path     string
	Rotation RotationPolicy
}

// ConcernPolicy describes optional concern-specific sink behavior.
type ConcernPolicy struct {
	Enabled  *bool
	Level    *slog.Level
	Rotation *RotationPolicy
}

// SetupPolicy is the resolved slogger setup contract. It is intentionally
// smaller than config.LoggingConfig so a separate resolver can own schema
// loading, defaulting, and validation.
type SetupPolicy struct {
	Level            slog.Level
	ProcessSink      FileSinkPolicy
	ConcernRoot      string
	ConcernPolicies  map[string]ConcernPolicy
	TranscriptPolicy TranscriptPolicy
	InventoryPolicy  InventoryPolicy
	CleanupPolicy    CleanupPolicy
}

// InventoryPolicy describes the lightweight inventory index sink.
type InventoryPolicy struct {
	Enabled  bool
	Root     string
	Rotation RotationPolicy
}

// CleanupPolicy controls retention across Clyde-owned log files.
type CleanupPolicy struct {
	Enabled bool
	Root    string
	// Roots lists additional owned directory trees.
	Roots []string
	// Paths lists exact owned files outside the roots, including their archives.
	Paths []string
	// KeepInPlacePaths lists open archives that cleanup must compact in place.
	KeepInPlacePaths []string
	// ProtectedPaths lists files counted in the budget but never changed.
	ProtectedPaths []string
	MaxAgeDays     int
	MaxBackups     int
	MaxTotalMB     int
}

// TranscriptPolicy describes the transcript router inputs that slogger needs.
type TranscriptPolicy struct {
	Enabled  bool
	Rotation RotationPolicy
}

func rotationConfig(policy RotationPolicy) gklog.RotationConfig {
	return gklog.RotationConfig{
		MaxSizeMB:  policy.MaxSizeMB,
		MaxBackups: policy.MaxBackups,
		MaxAgeDays: policy.MaxAgeDays,
		Compress:   policy.Compress,
	}
}
