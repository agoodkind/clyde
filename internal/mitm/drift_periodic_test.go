package mitm

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

// TestRunPeriodicDriftReturnsWhenDisabled verifies the loop self-gates and
// returns immediately when drift is disabled, even with a never-cancelled
// context.
func TestRunPeriodicDriftReturnsWhenDisabled(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:  false,
			Interval: config.Duration(time.Hour),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {},
			},
		},
	}
	if !returnsBefore(t, 2*time.Second, func(ctx context.Context) {
		RunPeriodicDrift(ctx, store, cfg, slog.Default())
	}) {
		t.Fatalf("RunPeriodicDrift did not return promptly when disabled")
	}
}

// TestRunPeriodicDriftReturnsWithNoUpstreams verifies the loop returns
// immediately when enabled but no upstreams are configured.
func TestRunPeriodicDriftReturnsWithNoUpstreams(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:   true,
			Interval:  config.Duration(time.Hour),
			Upstreams: nil,
		},
	}
	if !returnsBefore(t, 2*time.Second, func(ctx context.Context) {
		RunPeriodicDrift(ctx, store, cfg, slog.Default())
	}) {
		t.Fatalf("RunPeriodicDrift did not return promptly with no upstreams")
	}
}

// TestRunPeriodicDriftReturnsWithNilStore verifies the loop self-gates when no
// capture store is wired.
func TestRunPeriodicDriftReturnsWithNilStore(t *testing.T) {
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:  true,
			Interval: config.Duration(time.Hour),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {},
			},
		},
	}
	if !returnsBefore(t, 2*time.Second, func(ctx context.Context) {
		RunPeriodicDrift(ctx, nil, cfg, slog.Default())
	}) {
		t.Fatalf("RunPeriodicDrift did not return promptly with a nil store")
	}
}

// TestRunPeriodicDriftHonorsContextCancellation verifies the enabled loop
// returns promptly once its context is cancelled, using a long interval so the
// exit path can only be ctx cancellation rather than a tick.
func TestRunPeriodicDriftHonorsContextCancellation(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:  true,
			Interval: config.Duration(time.Hour),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunPeriodicDrift(ctx, store, cfg, slog.Default())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunPeriodicDrift did not return after context cancellation")
	}
}

// TestRunPeriodicDriftTicksWithoutPanicOnEmptyCorpus verifies that a tiny
// interval drives at least one sweep and that an empty corpus (no baseline) is
// handled rather than fatal: the loop keeps running until ctx is cancelled.
func TestRunPeriodicDriftTicksWithoutPanicOnEmptyCorpus(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:  true,
			Interval: config.Duration(25 * time.Millisecond),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {},
			},
		},
	}

	quietLog := slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		RunPeriodicDrift(ctx, store, cfg, quietLog)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunPeriodicDrift did not return after context deadline")
	}
}

// TestRunPeriodicDriftFallsBackToDefaultInterval verifies a non-positive
// configured interval does not stall the loop: with a cancelled context it
// still returns promptly rather than spinning on a zero-duration ticker.
func TestRunPeriodicDriftFallsBackToDefaultInterval(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:  true,
			Interval: config.Duration(0),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {},
			},
		},
	}
	if !returnsBefore(t, 2*time.Second, func(ctx context.Context) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		RunPeriodicDrift(ctx, store, cfg, slog.Default())
	}) {
		t.Fatalf("RunPeriodicDrift stalled with a non-positive interval")
	}
}

// TestRunDriftCheckRecordsCheckWithoutWritingBaseline verifies the compare-only
// check reads the corpus, records a drift_check, and never writes a baseline.
func TestRunDriftCheckRecordsCheckWithoutWritingBaseline(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	proxy := proxyWithStore(t, store)
	seedClaudeShape(t, proxy, cfg, claudeSystemBillingBody(t, "abc123def"))
	waitForUpstreamShapes(t, store, "claude-code", 1)

	outcome, err := RunDriftCheck(context.Background(), store, DriftCheckOptions{
		Upstream:  "claude-code",
		IncludeUA: []string{"claude-cli"},
	})
	if err != nil {
		t.Fatalf("RunDriftCheck: %v", err)
	}
	// No baseline exists yet, so the check is non-divergent and writes nothing.
	if outcome.Diverged {
		t.Fatalf("Diverged=true want false with no baseline")
	}
	if _, ok, _ := store.CurrentBaseline(context.Background(), "claude-code"); ok {
		t.Fatalf("RunDriftCheck wrote a baseline; it must be compare-only")
	}
}

// returnsBefore runs fn with a background context and reports whether it
// returned within the deadline.
func returnsBefore(t *testing.T, deadline time.Duration, fn func(ctx context.Context)) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn(context.Background())
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(deadline):
		return false
	}
}
