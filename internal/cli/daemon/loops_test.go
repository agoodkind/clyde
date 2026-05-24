package daemon

import (
	"bytes"
	"context"
	"log/slog"
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
