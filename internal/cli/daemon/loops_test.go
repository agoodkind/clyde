package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
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

func TestDriftLoopStartsDaemonOwnedMITMListenerWhenDefaultEnabled(t *testing.T) {
	configHome := t.TempDir()
	stateHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)

	globalDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatalf("mkdir global config: %v", err)
	}
	configPath := filepath.Join(globalDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[mitm]\nenabled_default = true\nproviders = [\"claude\"]\nbody_mode = \"summary\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cancel := driftLoop()(log)
	if cancel != nil {
		cancel()
	}

	out := buf.String()
	if !strings.Contains(out, "mitm.listener.daemon_owned") {
		t.Fatalf("expected daemon-owned MITM listener event, got: %s", out)
	}
	if !strings.Contains(out, filepath.Join(stateHome, "clyde", "mitm")) {
		t.Fatalf("expected XDG capture dir in listener event, got: %s", out)
	}
}

func TestDefaultDriftLogDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
	want := "/tmp/xdg-state/clyde/mitm-drift"
	if got := defaultDriftLogDir(); got != want {
		t.Errorf("defaultDriftLogDir XDG path: got %q want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	got := defaultDriftLogDir()
	if !strings.HasSuffix(got, ".local/state/clyde/mitm-drift") {
		t.Errorf("defaultDriftLogDir HOME path: got %q", got)
	}
}
