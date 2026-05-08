package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	adapteroauth "goodkind.io/clyde/internal/adapter/oauth"
	"goodkind.io/clyde/internal/config"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/prune"
	"goodkind.io/clyde/internal/session"
)

// pruneLoop returns a daemonsvc.ExtraLoop that runs the periodic
// session pruner when enabled in the global config. The loop ticks
// every cfg.Prune.Interval and runs the kinds (ephemeral, empty,
// autoname) that are individually toggled on. Output is discarded
// because the pruners log via slog.
func pruneLoop() daemonsvc.ExtraLoop {
	return func(log *slog.Logger) func() {
		cfg, err := config.LoadGlobalOrDefault()
		if err != nil {
			log.LogAttrs(context.Background(), slog.LevelWarn, "prune.config_load_failed",
				slog.String("component", "prune"),
				slog.Any("err", err),
			)
			return nil
		}
		if !cfg.Prune.Enabled {
			return nil
		}
		interval := cfg.Prune.Interval
		if interval <= 0 {
			interval = time.Hour
		}
		emptySettings := prune.DefaultEmptySettings()
		if cfg.Prune.EmptyMaxLines > 0 {
			emptySettings.MaxLines = cfg.Prune.EmptyMaxLines
		}
		if cfg.Prune.EmptyMinAge > 0 {
			emptySettings.MinAge = cfg.Prune.EmptyMinAge
		}
		autonameMinAge := cfg.Prune.AutonameMinAge
		if autonameMinAge <= 0 {
			autonameMinAge = 7 * 24 * time.Hour
		}

		log.LogAttrs(context.Background(), slog.LevelInfo, "prune.tick.scheduled",
			slog.String("component", "prune"),
			slog.Int64("interval_ms", interval.Milliseconds()),
			slog.Bool("ephemeral", cfg.Prune.Ephemeral),
			slog.Bool("empty", cfg.Prune.Empty),
			slog.Bool("autoname", cfg.Prune.Autoname),
		)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.WarnContext(ctx, "prune.loop.panicked",
						"component", "prune",
						"panic", r,
					)
				}
			}()
			ticker := time.NewTicker(interval)
			defer func() { ticker.Stop() }()
			runPruneTick(ctx, log, cfg.Prune, emptySettings, autonameMinAge)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runPruneTick(ctx, log, cfg.Prune, emptySettings, autonameMinAge)
				}
			}
		}()
		return cancel
	}
}

func runPruneTick(
	ctx context.Context,
	log *slog.Logger,
	cfg config.PruneConfig,
	empty prune.EmptySettings,
	autonameMinAge time.Duration,
) {
	store, err := session.NewGlobalFileStore()
	if err != nil {
		log.LogAttrs(ctx, slog.LevelWarn, "prune.tick.store_init_failed",
			slog.String("component", "prune"),
			slog.Any("err", err),
		)
		return
	}
	opts := prune.Options{
		DryRun:         false,
		SkipConfirm:    true,
		Empty:          empty,
		AutonameMinAge: autonameMinAge,
	}
	if cfg.Ephemeral {
		runOnePrune(ctx, log, store, prune.KindEphemeral, opts)
	}
	if cfg.Empty {
		runOnePrune(ctx, log, store, prune.KindEmpty, opts)
	}
	if cfg.Autoname {
		runOnePrune(ctx, log, store, prune.KindAutoname, opts)
	}
}

func runOnePrune(
	ctx context.Context,
	log *slog.Logger,
	store session.Store,
	kind prune.Kind,
	opts prune.Options,
) {
	started := cliDaemonNow()
	res, err := prune.Run(ctx, kind, store, log, io.Discard, opts)
	elapsed := time.Since(started)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "prune.tick.failed",
			slog.String("component", "prune"),
			slog.String("kind", string(kind)),
			slog.Int64("duration_ms", elapsed.Milliseconds()),
			slog.Any("err", err),
		)
		return
	}
	log.LogAttrs(ctx, slog.LevelInfo, "prune.tick.completed",
		slog.String("component", "prune"),
		slog.String("kind", string(kind)),
		slog.Int("considered", res.Considered),
		slog.Int("pruned", res.Pruned),
		slog.Int("failures", len(res.Failures)),
		slog.Int64("duration_ms", elapsed.Milliseconds()),
	)
}

// oauthLoop returns a daemonsvc.ExtraLoop that periodically refreshes
// the Anthropic OAuth access token so the adapter's direct-OAuth path
// almost never has to refresh inline. Defaults on; user can disable
// via [oauth] disabled = true in the global config.
func oauthLoop() daemonsvc.ExtraLoop {
	return func(log *slog.Logger) func() {
		cfg, err := config.LoadGlobalOrDefault()
		if err != nil {
			log.LogAttrs(context.Background(), slog.LevelWarn, "oauth.config_load_failed",
				slog.String("component", "oauth"),
				slog.Any("err", err),
			)
			return nil
		}
		if !cfg.OAuth.IsEnabled() {
			return nil
		}
		if err := cfg.Adapter.OAuth.ValidateOAuthFields(); err != nil {
			log.LogAttrs(context.Background(), slog.LevelWarn, "oauth.refresher.skipped_incomplete_adapter_oauth",
				slog.String("component", "oauth"),
				slog.Any("err", err),
			)
			return nil
		}
		interval := cfg.OAuth.Interval
		if interval <= 0 {
			interval = 4 * time.Hour
		}

		log.LogAttrs(context.Background(), slog.LevelInfo, "oauth.refresher.scheduled",
			slog.String("component", "oauth"),
			slog.Int64("interval_ms", interval.Milliseconds()),
		)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.WarnContext(ctx, "oauth.refresher.panicked",
						"component", "oauth",
						"panic", r,
					)
				}
			}()
			ticker := time.NewTicker(interval)
			defer func() { ticker.Stop() }()
			runOAuthRefresh(ctx, log)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runOAuthRefresh(ctx, log)
				}
			}
		}()
		return cancel
	}
}

// driftLoop returns a daemonsvc.ExtraLoop that periodically refreshes
// local MITM baselines from the daemon-owned capture store. Disabled
// by default; enable via [mitm.drift] enabled = true in the global
// config.
func driftLoop() daemonsvc.ExtraLoop {
	return func(log *slog.Logger) func() {
		cfg, err := config.LoadGlobalOrDefault()
		if err != nil {
			log.LogAttrs(context.Background(), slog.LevelWarn, "mitm.drift.config_load_failed",
				slog.String("component", "mitm-drift"),
				slog.Any("err", err),
			)
			return nil
		}
		dcfg := cfg.MITM.Drift
		if !dcfg.Enabled || len(dcfg.Upstreams) == 0 {
			return nil
		}
		interval := dcfg.Interval
		if interval <= 0 {
			interval = 24 * time.Hour
		}
		upstreams := make([]string, 0, len(dcfg.Upstreams))
		for name := range dcfg.Upstreams {
			upstreams = append(upstreams, name)
		}
		sort.Strings(upstreams)

		log.LogAttrs(context.Background(), slog.LevelInfo, "mitm.drift.scheduled",
			slog.String("component", "mitm-drift"),
			slog.Int64("interval_ms", interval.Milliseconds()),
			slog.Any("upstreams", upstreams),
		)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.WarnContext(ctx, "mitm.drift.loop.panicked",
						"component", "mitm-drift",
						"panic", r,
					)
				}
			}()
			ticker := time.NewTicker(interval)
			defer func() { ticker.Stop() }()
			runDriftTick(ctx, log, cfg.MITM, dcfg, upstreams)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runDriftTick(ctx, log, cfg.MITM, dcfg, upstreams)
				}
			}
		}()
		return cancel
	}
}

// driftTickSummary aggregates one tick's outcomes so the loop can
// emit a single Info summary at the end (per the no-info-in-loops
// rule). Per-upstream divergence and failure events still fire at
// Warn/Error so they are not suppressed.
type driftTickSummary struct {
	totalDurationMs int64
	clean           []string
	diverged        []string
	failed          []string
	skipped         []string
}

func runDriftTick(
	ctx context.Context,
	log *slog.Logger,
	mcfg config.MITMConfig,
	dcfg config.MITMDriftConfig,
	upstreams []string,
) {
	logDir := strings.TrimSpace(dcfg.DriftLogDir)
	if logDir == "" {
		logDir = defaultDriftLogDir()
	}
	tickStarted := cliDaemonNow()
	summary := driftTickSummary{}
	captureRoot := strings.TrimSpace(dcfg.CaptureRoot)
	if captureRoot == "" {
		captureRoot = strings.TrimSpace(mcfg.CaptureDir)
	}
	for _, upstream := range upstreams {
		entry := dcfg.Upstreams[upstream]
		outcome, err := mitm.RefreshBaseline(ctx, mitm.BaselineRefreshOptions{
			Upstream:        upstream,
			CaptureRoot:     captureRoot,
			Reference:       entry.Reference,
			DriftLogPath:    filepath.Join(logDir, upstream+".jsonl"),
			IncludeUA:       entry.IncludeUA,
			ExcludeUA:       entry.ExcludeUA,
			RequireBodyKeys: entry.RequireBodyKeys,
			ForbidBodyKeys:  entry.ForbidBodyKeys,
			Log:             log.With("subcomponent", "mitm-drift", "upstream", upstream),
		})
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return
		case err != nil:
			log.LogAttrs(ctx, slog.LevelError, "mitm.drift.tick_failed",
				slog.String("component", "mitm-drift"),
				slog.String("upstream", upstream),
				slog.Any("err", err),
			)
			summary.failed = append(summary.failed, upstream)
		case outcome.Diverged:
			log.LogAttrs(ctx, slog.LevelWarn, "mitm.drift.tick_diverged",
				slog.String("component", "mitm-drift"),
				slog.String("upstream", upstream),
				slog.String("schema_version", outcome.SchemaVersion),
				slog.String("summary", outcome.Summary),
			)
			summary.diverged = append(summary.diverged, upstream)
		default:
			summary.clean = append(summary.clean, upstream)
		}
	}
	summary.totalDurationMs = time.Since(tickStarted).Milliseconds()
	log.LogAttrs(ctx, slog.LevelInfo, "mitm.drift.tick_completed",
		slog.String("component", "mitm-drift"),
		slog.Int64("duration_ms", summary.totalDurationMs),
		slog.Int("clean", len(summary.clean)),
		slog.Int("diverged", len(summary.diverged)),
		slog.Int("failed", len(summary.failed)),
		slog.Int("skipped", len(summary.skipped)),
	)
}

func defaultDriftLogDir() string {
	if base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); base != "" {
		return filepath.Join(base, "clyde", "mitm-drift")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "mitm-drift"
	}
	return filepath.Join(home, ".local", "state", "clyde", "mitm-drift")
}

func runOAuthRefresh(ctx context.Context, log *slog.Logger) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		log.LogAttrs(ctx, slog.LevelWarn, "oauth.refresh.config_failed",
			slog.String("component", "oauth"),
			slog.Any("err", err),
		)
		return
	}
	mgr := adapteroauth.NewManager(cfg.Adapter.OAuth, "")
	timeoutCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	started := cliDaemonNow()
	token, err := mgr.Token(timeoutCtx)
	elapsed := time.Since(started)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "oauth.refresh.failed",
			slog.String("component", "oauth"),
			slog.Int64("duration_ms", elapsed.Milliseconds()),
			slog.Any("err", err),
		)
		return
	}
	log.LogAttrs(ctx, slog.LevelInfo, "oauth.refresh.completed",
		slog.String("component", "oauth"),
		slog.Int64("duration_ms", elapsed.Milliseconds()),
		slog.Int("token_len", len(token)),
	)
}
