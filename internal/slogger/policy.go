package slogger

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

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
	Level             slog.Level
	ProcessSink       FileSinkPolicy
	ConcernRoot       string
	ConcernPolicies   map[string]ConcernPolicy
	TranscriptPolicy  TranscriptPolicy
	InventoryPolicy   InventoryPolicy
	MITMCapturePolicy MITMCapturePolicy
	CleanupPolicy     CleanupPolicy
}

// InventoryPolicy describes the lightweight inventory index sink.
type InventoryPolicy struct {
	Enabled  bool
	Root     string
	Rotation RotationPolicy
}

// CleanupPolicy describes startup cleanup for rotated log surfaces.
type CleanupPolicy struct {
	Enabled    bool
	Root       string
	MaxAgeDays int
	MaxBackups int
	MaxTotalMB int
}

// TranscriptPolicy describes the transcript router inputs that slogger needs.
type TranscriptPolicy struct {
	Enabled bool
}

// KnownConcern reports whether name is a registered concern key.
func KnownConcern(name string) bool {
	_, ok := concernPaths[strings.TrimSpace(name)]
	return ok
}

// KnownConcerns returns the registered concern keys in stable order.
func KnownConcerns() []string {
	concerns := make([]string, 0, len(concernPaths))
	for concern := range concernPaths {
		concerns = append(concerns, concern)
	}
	slices.Sort(concerns)
	return concerns
}

// ValidateConcernNames rejects unknown concern keys. The input names are
// concern names such as "adapter.http.ingress", not legacy event prefixes.
func ValidateConcernNames(names []string) error {
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if !KnownConcern(trimmed) {
			return fmt.Errorf("slogger: unknown concern %q", name)
		}
	}
	return nil
}

func validateConcernPolicyNames(policies map[string]ConcernPolicy) error {
	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	return ValidateConcernNames(names)
}

func rotationConfig(policy RotationPolicy) gklog.RotationConfig {
	return gklog.RotationConfig{
		MaxSizeMB:  policy.MaxSizeMB,
		MaxBackups: policy.MaxBackups,
		MaxAgeDays: policy.MaxAgeDays,
		Compress:   policy.Compress,
	}
}
