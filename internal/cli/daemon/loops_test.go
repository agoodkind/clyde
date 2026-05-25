package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/slogger"
)

func TestRunDriftTickFallsBackToLocalBaselineWithoutReference(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	captureDir := t.TempDir()
	dcfg := config.MITMDriftConfig{
		Enabled:     true,
		DriftLogDir: t.TempDir(),
		Upstreams: map[string]config.MITMDriftUpstreamCfg{
			"claude-code": {Reference: ""},
		},
	}
	runDriftTick(context.Background(), log, config.MITMConfig{CaptureDir: captureDir}, dcfg, []string{"claude-code"})
	out := buf.String()
	if !strings.Contains(out, "mitm.drift.tick_failed") {
		t.Errorf("expected failure event when no capture exists, got: %s", out)
	}
	if strings.Contains(out, "mitm.drift.upstream_skipped_no_reference") {
		t.Errorf("expected empty reference to use local baseline fallback, got: %s", out)
	}
}

func TestSlogCleanupLoopReturnsNilWhenDisabled(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Confirm the default daemon policy is enabled so the disabled case below
	// proves the loop honored the override and did not start a goroutine.
	enabledPolicy, err := defaultDaemonCleanupPolicy(t)
	if err != nil {
		t.Fatalf("resolve default policy: %v", err)
	}
	if !enabledPolicy.Enabled {
		t.Fatalf("default daemon cleanup policy should be enabled; got %+v", enabledPolicy)
	}

	disable := false
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Logging.Cleanup.Enabled = &disable
	if err := writeGlobalConfig(t, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cancel := slogCleanupLoop()(log)
	if cancel != nil {
		cancel()
		t.Fatalf("slogCleanupLoop returned non-nil cancel when disabled")
	}
	if strings.Contains(buf.String(), "slogger.cleanup.tick.scheduled") {
		t.Fatalf("disabled loop must not log tick.scheduled: %s", buf.String())
	}
}

func TestSlogCleanupLoopReturnsCancelWhenEnabled(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cancel := slogCleanupLoop()(log)
	if cancel == nil {
		t.Fatalf("slogCleanupLoop returned nil cancel when enabled by default")
	}
	cancel()
	// Calling cancel a second time must not panic; context cancel is
	// idempotent and the loop relies on that for safe double-stop.
	cancel()
}

func defaultDaemonCleanupPolicy(t *testing.T) (slogger.CleanupPolicy, error) {
	t.Helper()
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return slogger.CleanupPolicy{}, err
	}
	setupPolicy, err := resolveDaemonSetupPolicy(*cfg)
	if err != nil {
		return slogger.CleanupPolicy{}, err
	}
	return setupPolicy.CleanupPolicy, nil
}

func resolveDaemonSetupPolicy(cfg config.Config) (slogger.SetupPolicy, error) {
	return logpolicy.ResolveSloggerSetup(cfg, slogger.ProcessRoleDaemon)
}

func writeGlobalConfig(t *testing.T, cfg *config.Config) error {
	t.Helper()
	// LoadGlobalOrDefault returns defaults when no global config file exists,
	// so the disabled-loop case can be exercised by mutating the in-memory
	// config and writing it to the config path the loop will re-read on its
	// fresh LoadGlobalOrDefault call.
	return config.SaveGlobal(cfg)
}

func TestDefaultDriftLogDir(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	want := filepath.Join(stateRoot, "clyde", "mitm-drift")
	if got := defaultDriftLogDir(); got != want {
		t.Errorf("defaultDriftLogDir XDG path: got %q want %q", got, want)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "~/state/../state-root")
	want = filepath.Join(home, "state-root", "clyde", "mitm-drift")
	if got := defaultDriftLogDir(); got != want {
		t.Errorf("defaultDriftLogDir expanded XDG path: got %q want %q", got, want)
	}

	t.Setenv("XDG_STATE_HOME", "")
	got := defaultDriftLogDir()
	if !strings.HasSuffix(got, ".local/state/clyde/mitm-drift") {
		t.Errorf("defaultDriftLogDir HOME path: got %q", got)
	}
}
