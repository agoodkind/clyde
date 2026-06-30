package mitm

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestSeedBaselineWritesStoredBaselineFromShapes(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	proxy := proxyWithStore(t, store)
	header := http.Header{}
	header.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	header.Set("Anthropic-Version", "2023-06-01")
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      header,
		body:        claudeSystemBillingBody(t, "cch-seed"),
	})
	waitForUpstreamShapes(t, store, "claude-code", 1)

	result, err := SeedBaseline(context.Background(), store, "claude-code", []string{"claude-cli"}, nil)
	if err != nil {
		t.Fatalf("SeedBaseline: %v", err)
	}
	if result.Upstream != "claude-code" {
		t.Fatalf("upstream=%q want claude-code", result.Upstream)
	}
	if result.Flavors < 1 {
		t.Fatalf("flavor count=%d want at least 1", result.Flavors)
	}

	raw, ok, err := store.CurrentBaseline(context.Background(), "claude-code")
	if err != nil || !ok {
		t.Fatalf("CurrentBaseline: ok=%v err=%v want a stored baseline", ok, err)
	}
	snap, err := unmarshalSnapshot(raw)
	if err != nil {
		t.Fatalf("decode baseline: %v", err)
	}
	if snap.Upstream.Name != "claude-code" {
		t.Fatalf("upstream name=%q want claude-code", snap.Upstream.Name)
	}
	if len(snap.Flavors) < 1 {
		t.Fatalf("flavor count=%d want at least 1", len(snap.Flavors))
	}
	if snap.Flavors[0].Signature.UserAgent != "claude-cli/2.1.123 (external, sdk-cli)" {
		t.Fatalf("flavor user agent=%q", snap.Flavors[0].Signature.UserAgent)
	}
}

func TestSeedBaselineRequiresUpstreamAndStore(t *testing.T) {
	store := openTestCaptureStore(t)
	if _, err := SeedBaseline(context.Background(), store, "", nil, nil); err == nil {
		t.Fatal("expected error when upstream is empty")
	}
	if _, err := SeedBaseline(context.Background(), nil, "claude-code", nil, nil); err == nil {
		t.Fatal("expected error when store is nil")
	}
}

func TestSeedBaselineErrorsWhenCorpusEmpty(t *testing.T) {
	store := openTestCaptureStore(t)
	if _, err := SeedBaseline(context.Background(), store, "claude-code", nil, nil); err == nil {
		t.Fatal("expected error when the shape corpus is empty")
	}
}

// TestHardCutoverWritesNoLegacyFiles is the hard-cutover guard: after a full
// capture + refresh cycle against the store, no JSONL/TOML drift or baseline
// files exist under the state dir, and the baseline lives only in capture.db.
func TestHardCutoverWritesNoLegacyFiles(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	proxy := proxyWithStore(t, store)
	header := http.Header{}
	header.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	header.Set("Anthropic-Version", "2023-06-01")
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      header,
		body:        claudeSystemBillingBody(t, "cch-cutover"),
	})
	waitForUpstreamShapes(t, store, "claude-code", 1)

	if _, err := RefreshBaseline(context.Background(), store, BaselineRefreshOptions{Upstream: "claude-code", IncludeUA: []string{"claude-cli"}}); err != nil {
		t.Fatalf("RefreshBaseline: %v", err)
	}

	stateRoot := config.DefaultStateDir()
	for _, legacy := range []string{
		filepath.Join(stateRoot, "mitm", "drift"),
		filepath.Join(stateRoot, "mitm-drift"),
		filepath.Join(stateRoot, "mitm-baselines"),
		filepath.Join(stateRoot, "mitm-baselines", "claude-code", "baseline-reference.toml"),
		filepath.Join(stateRoot, "mitm", "drift", "claude-code.jsonl"),
	} {
		if _, err := os.Stat(legacy); !os.IsNotExist(err) {
			t.Fatalf("legacy drift/baseline path exists after cutover: %s (stat err=%v)", legacy, err)
		}
	}

	if _, ok, err := store.CurrentBaseline(context.Background(), "claude-code"); err != nil || !ok {
		t.Fatalf("baseline not stored in capture.db after refresh: ok=%v err=%v", ok, err)
	}
}
