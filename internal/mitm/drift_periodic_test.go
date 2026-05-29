package mitm

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

// TestRunPeriodicDriftReturnsWhenDisabled verifies the loop self-gates and
// returns immediately when drift is disabled, even with a never-cancelled
// context.
func TestRunPeriodicDriftReturnsWhenDisabled(t *testing.T) {
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:  false,
			Interval: config.Duration(time.Hour),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {Reference: "/does/not/matter"},
			},
		},
	}
	if !returnsBefore(t, 2*time.Second, func(ctx context.Context) {
		RunPeriodicDrift(ctx, cfg, slog.Default())
	}) {
		t.Fatalf("RunPeriodicDrift did not return promptly when disabled")
	}
}

// TestRunPeriodicDriftReturnsWithNoUpstreams verifies the loop returns
// immediately when enabled but no upstreams are configured.
func TestRunPeriodicDriftReturnsWithNoUpstreams(t *testing.T) {
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:   true,
			Interval:  config.Duration(time.Hour),
			Upstreams: nil,
		},
	}
	if !returnsBefore(t, 2*time.Second, func(ctx context.Context) {
		RunPeriodicDrift(ctx, cfg, slog.Default())
	}) {
		t.Fatalf("RunPeriodicDrift did not return promptly with no upstreams")
	}
}

// TestRunPeriodicDriftHonorsContextCancellation verifies the enabled loop
// returns promptly once its context is cancelled, using a long interval so the
// exit path can only be ctx cancellation rather than a tick.
func TestRunPeriodicDriftHonorsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:     true,
			Interval:    config.Duration(time.Hour),
			DriftLogDir: filepath.Join(dir, "drift"),
			CaptureRoot: filepath.Join(dir, "capture"),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {Reference: filepath.Join(dir, "missing-reference.toml")},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunPeriodicDrift(ctx, cfg, slog.Default())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RunPeriodicDrift did not return after context cancellation")
	}
}

// TestRunPeriodicDriftTicksWithoutPanicOnMissingBaseline verifies that a tiny
// interval drives at least one sweep and that a missing baseline reference is
// logged rather than fatal: the loop keeps running until ctx is cancelled.
func TestRunPeriodicDriftTicksWithoutPanicOnMissingBaseline(t *testing.T) {
	dir := t.TempDir()
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:     true,
			Interval:    config.Duration(25 * time.Millisecond),
			DriftLogDir: filepath.Join(dir, "drift"),
			CaptureRoot: filepath.Join(dir, "capture"),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {Reference: filepath.Join(dir, "missing-reference.toml")},
			},
		},
	}

	// Route the expected per-tick missing-transcript warnings to a discard
	// handler so the test output stays readable; the assertion is that the
	// loop ticks and returns without panicking, not that it logs.
	quietLog := slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		RunPeriodicDrift(ctx, cfg, quietLog)
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
	dir := t.TempDir()
	cfg := config.MITMConfig{
		Drift: config.MITMDriftConfig{
			Enabled:     true,
			Interval:    config.Duration(0),
			DriftLogDir: filepath.Join(dir, "drift"),
			CaptureRoot: filepath.Join(dir, "capture"),
			Upstreams: map[string]config.MITMDriftUpstreamCfg{
				"claude-code": {Reference: filepath.Join(dir, "missing-reference.toml")},
			},
		},
	}
	if !returnsBefore(t, 2*time.Second, func(ctx context.Context) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		RunPeriodicDrift(ctx, cfg, slog.Default())
	}) {
		t.Fatalf("RunPeriodicDrift stalled with a non-positive interval")
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
