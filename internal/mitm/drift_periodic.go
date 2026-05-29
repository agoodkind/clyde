package mitm

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
)

// defaultPeriodicDriftInterval is the fallback tick interval used when the
// configured drift interval is non-positive.
const defaultPeriodicDriftInterval = 5 * time.Minute

// RunPeriodicDrift runs the daemon-owned periodic drift loop. It ticks on
// cfg.Drift.Interval and, on each tick, runs a compare-only drift check for
// every configured upstream against the current local capture store.
//
// The loop self-gates: if drift is disabled or no upstreams are configured it
// returns immediately, so the caller can spawn it unconditionally. A single
// upstream's infrastructure error (for example a missing baseline reference or
// an empty transcript) is logged at warn and does not stop the loop. The loop
// returns promptly when ctx is cancelled.
func RunPeriodicDrift(ctx context.Context, cfg config.MITMConfig, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	dcfg := cfg.Drift
	if !dcfg.Enabled || len(dcfg.Upstreams) == 0 {
		return
	}

	interval := dcfg.Interval.AsDuration()
	if interval <= 0 {
		interval = defaultPeriodicDriftInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runDriftSweep(ctx, cfg, log)
		}
	}
}

// runDriftSweep performs one drift check for each configured upstream. It is a
// best-effort sweep: per-upstream infrastructure errors are logged and the
// sweep continues with the next upstream.
func runDriftSweep(ctx context.Context, cfg config.MITMConfig, log *slog.Logger) {
	dcfg := cfg.Drift

	captureRoot := strings.TrimSpace(dcfg.CaptureRoot)
	if captureRoot == "" {
		captureRoot = strings.TrimSpace(cfg.CaptureDir)
	}
	if captureRoot == "" {
		captureRoot = DefaultCaptureRoot()
	}
	logDir := strings.TrimSpace(dcfg.DriftLogDir)
	if logDir == "" {
		logDir = DefaultDriftLogDir()
	}

	for upstream, entry := range dcfg.Upstreams {
		select {
		case <-ctx.Done():
			return
		default:
		}
		opts := DriftCheckOptions{
			Upstream:        upstream,
			Reference:       entry.Reference,
			CaptureRoot:     captureRoot,
			CACertPath:      dcfg.CACertPath,
			DriftLogPath:    filepath.Join(logDir, upstream+".jsonl"),
			IncludeUA:       entry.IncludeUA,
			ExcludeUA:       entry.ExcludeUA,
			RequireBodyKeys: entry.RequireBodyKeys,
			ForbidBodyKeys:  entry.ForbidBodyKeys,
			Log:             log.With("upstream", upstream),
		}
		outcome, err := RunDriftCheck(ctx, opts)
		if err != nil {
			log.WarnContext(ctx, "mitm.drift.periodic_check_failed", "concern", "providers.mitm.wire",
				"upstream", upstream,
				"err", err,
			)
			continue
		}
		if outcome.Diverged {
			log.WarnContext(ctx, "mitm.drift.periodic_diverged", "concern", "providers.mitm.wire",
				"upstream", upstream,
				"schema_version", outcome.SchemaVersion,
				"summary", outcome.Summary,
			)
		}
	}
}
