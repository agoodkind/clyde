package mitm

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/config"
)

const baselineRefreshDebounce = 2 * time.Second

type baselineRefresher struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

var defaultBaselineRefresher = &baselineRefresher{
	timers: map[string]*time.Timer{},
}

func queueBaselineRefresh(ctx context.Context, cfg config.MITMConfig, provider string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	dcfg := cfg.Drift
	if !dcfg.Enabled || len(dcfg.Upstreams) == 0 {
		return
	}
	captureRoot := strings.TrimSpace(cfg.CaptureDir)
	if captureRoot == "" {
		captureRoot = DefaultCaptureRoot()
	}
	logDir := strings.TrimSpace(dcfg.DriftLogDir)
	if logDir == "" {
		logDir = DefaultDriftLogDir()
	}
	for upstream, entry := range dcfg.Upstreams {
		if !upstreamMatchesProvider(upstream, provider) {
			continue
		}
		opts := BaselineRefreshOptions{
			Upstream:        upstream,
			CaptureRoot:     captureRoot,
			Reference:       entry.Reference,
			DriftLogPath:    filepath.Join(logDir, upstream+".jsonl"),
			IncludeUA:       entry.IncludeUA,
			ExcludeUA:       entry.ExcludeUA,
			RequireBodyKeys: entry.RequireBodyKeys,
			ForbidBodyKeys:  entry.ForbidBodyKeys,
			Log:             log.With("upstream", upstream, "provider", provider),
		}
		defaultBaselineRefresher.schedule(ctx, opts)
	}
}

func (r *baselineRefresher) schedule(ctx context.Context, opts BaselineRefreshOptions) {
	if r == nil {
		return
	}
	key := strings.TrimSpace(opts.Upstream)
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.timers[key]; existing != nil {
		existing.Stop()
	}
	// Detach the context for the scheduled work so the timer survives
	// the originating request. context.WithoutCancel preserves values
	// (correlation IDs, slog handlers) without inheriting deadlines.
	scheduledCtx := context.WithoutCancel(ctx)
	r.timers[key] = time.AfterFunc(baselineRefreshDebounce, func() {
		outcome, err := RefreshBaseline(scheduledCtx, opts)
		if err != nil {
			opts.Log.WarnContext(scheduledCtx, "mitm.baseline.refresh_failed", "err", err)
		} else if outcome.Updated {
			level := slog.LevelInfo
			if outcome.Diverged {
				level = slog.LevelWarn
			}
			opts.Log.LogAttrs(scheduledCtx, level, "mitm.baseline.refreshed",
				slog.String("schema_version", outcome.SchemaVersion),
				slog.String("baseline_path", outcome.BaselinePath),
				slog.Bool("created", outcome.Created),
				slog.Bool("diverged", outcome.Diverged),
				slog.String("summary", outcome.Summary),
			)
		}
		r.mu.Lock()
		delete(r.timers, key)
		r.mu.Unlock()
	})
}

func upstreamMatchesProvider(upstream string, provider string) bool {
	want := ProviderForUpstream(upstream)
	if want == "" {
		return true
	}
	return strings.EqualFold(want, strings.TrimSpace(provider))
}
