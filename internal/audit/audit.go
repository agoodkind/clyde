// Package audit provides a shared slog.Logger that writes structured JSON
// to ~/.local/state/clyde/audit.jsonl. All processes (CLI, MCP server,
// daemon) write to the same file via rotating append writes.
package audit

import (
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog"
	gklogversion "goodkind.io/gklog/version"
)

const auditFile = "audit.jsonl"

// RotationConfig controls rotation for the audit.jsonl sink. Mirrors
// the logpolicy.RotationPolicy shape so callers can resolve rotation
// from the SinkAudit policy without importing logpolicy here.
type RotationConfig struct {
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
}

const (
	defaultAuditRotationMaxSizeMB  = 64
	defaultAuditRotationMaxBackups = 192
	defaultAuditRotationMaxAgeDays = 14
)

func (c RotationConfig) normalized() RotationConfig {
	if c.MaxSizeMB <= 0 {
		c.MaxSizeMB = defaultAuditRotationMaxSizeMB
	}
	if c.MaxBackups <= 0 {
		c.MaxBackups = defaultAuditRotationMaxBackups
	}
	if c.MaxAgeDays <= 0 {
		c.MaxAgeDays = defaultAuditRotationMaxAgeDays
	}
	return c
}

// NewLogger creates an slog.Logger that writes JSON to stderr and the audit
// log file with rotation. The rotation policy is supplied by the caller so
// the logpolicy boundary owns the budget. Returns the logger and a cleanup
// function.
func NewLogger(component string, rotation RotationConfig) (*slog.Logger, func()) {
	stateDir := config.DefaultStateDir()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return stderrFallback(component), func() {}
	}

	logPath := filepath.Join(stateDir, auditFile)
	jsonOpts := &slog.HandlerOptions{Level: slog.LevelDebug}

	rotation = rotation.normalized()
	compressCopy := rotation.Compress
	lj := gklog.NewLumberjackWriterWithConfig(logPath, gklog.RotationConfig{
		MaxSizeMB:  rotation.MaxSizeMB,
		MaxBackups: rotation.MaxBackups,
		MaxAgeDays: rotation.MaxAgeDays,
		Compress:   &compressCopy,
	})
	stderrH := slog.NewJSONHandler(os.Stderr, jsonOpts)
	fileH := slog.NewJSONHandler(lj, jsonOpts)
	logger := slog.New(gklog.NewTeeHandler(stderrH, fileH)).
		With(slog.String("build", gklogversion.String())).
		With("component", component)

	return logger, func() { _ = lj.Close() }
}

func stderrFallback(component string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})).With(slog.String("build", gklogversion.String())).With("component", component)
}
