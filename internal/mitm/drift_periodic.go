package mitm

import (
	"context"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm/capture"
)

// defaultPeriodicDriftInterval is the fallback tick interval used when the
// configured drift interval is non-positive.
const defaultPeriodicDriftInterval = 5 * time.Minute

// RunPeriodicDrift runs the daemon-owned periodic drift loop. It ticks on
// cfg.Drift.Interval and, on each tick, runs a compare-only drift check for
// every configured upstream against the capture store's deduped shape corpus.
//
// The loop self-gates: if drift is disabled, no upstreams are configured, or
// no store is wired it returns immediately, so the caller can spawn it
// unconditionally. A single upstream's infrastructure error is logged at warn
// and does not stop the loop. The loop returns promptly when ctx is cancelled.
func RunPeriodicDrift(ctx context.Context, store *capture.Store, cfg config.MITMConfig, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	dcfg := cfg.Drift
	if !dcfg.Enabled || len(dcfg.Upstreams) == 0 || store == nil {
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
			runDriftSweep(ctx, store, cfg, log)
		}
	}
}

// runDriftSweep performs one compare-only drift check for each configured
// upstream against the store. It is best-effort: per-upstream infrastructure
// errors are logged and the sweep continues with the next upstream.
func runDriftSweep(ctx context.Context, store *capture.Store, cfg config.MITMConfig, log *slog.Logger) {
	dcfg := cfg.Drift
	for upstream, entry := range dcfg.Upstreams {
		select {
		case <-ctx.Done():
			return
		default:
		}
		opts := DriftCheckOptions{
			Upstream:        upstream,
			IncludeUA:       entry.IncludeUA,
			ExcludeUA:       entry.ExcludeUA,
			RequireBodyKeys: entry.RequireBodyKeys,
			ForbidBodyKeys:  entry.ForbidBodyKeys,
			Log:             log.With("upstream", upstream),
		}
		outcome, err := RunDriftCheck(ctx, store, opts)
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
				"summary", outcome.Summary,
			)
		}
	}
}
