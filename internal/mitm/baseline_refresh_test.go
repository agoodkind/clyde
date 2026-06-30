package mitm

import (
	"context"
	"net/http"
	"testing"

	"goodkind.io/clyde/internal/config"
)

// seedClaudeShape records one claude drift shape into store via the proxy's
// drift-capture path, so the corpus carries a deduped shape the refresh reads.
func seedClaudeShape(t *testing.T, proxy *Proxy, cfg config.MITMConfig, body []byte) {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer secret")
	header.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	header.Set("Anthropic-Beta", "oauth-2025-04-20,claude-code-20250219")
	header.Set("Anthropic-Version", "2023-06-01")
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      header,
		body:        body,
	})
}

func TestRefreshBaselineDegradesWhenCorpusEmpty(t *testing.T) {
	store := openTestCaptureStore(t)
	outcome, err := RefreshBaseline(context.Background(), store, BaselineRefreshOptions{
		Upstream: "claude-code",
	})
	if err != nil {
		t.Fatalf("RefreshBaseline: want nil error on empty corpus, got %v", err)
	}
	if outcome.Created || outcome.Updated {
		t.Fatalf("Created=%v Updated=%v want both false on empty corpus", outcome.Created, outcome.Updated)
	}
	if _, ok, _ := store.CurrentBaseline(context.Background(), "claude-code"); ok {
		t.Fatalf("baseline written for empty corpus; want none")
	}
}

func TestRefreshBaselineInitializesStoredBaseline(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	proxy := proxyWithStore(t, store)
	seedClaudeShape(t, proxy, cfg, claudeSystemBillingBody(t, "abc123def"))
	waitForUpstreamShapes(t, store, "claude-code", 1)

	outcome, err := RefreshBaseline(context.Background(), store, BaselineRefreshOptions{
		Upstream:  "claude-code",
		IncludeUA: []string{"claude-cli"},
	})
	if err != nil {
		t.Fatalf("RefreshBaseline: %v", err)
	}
	if !outcome.Created {
		t.Fatalf("Created=false want true")
	}
	if !outcome.Updated {
		t.Fatalf("Updated=false want true")
	}

	raw, ok, err := store.CurrentBaseline(context.Background(), "claude-code")
	if err != nil || !ok {
		t.Fatalf("CurrentBaseline: ok=%v err=%v want a stored baseline", ok, err)
	}
	snap, err := unmarshalSnapshot(raw)
	if err != nil {
		t.Fatalf("decode baseline: %v", err)
	}
	if len(snap.Flavors) != 1 {
		t.Fatalf("flavor count=%d want 1", len(snap.Flavors))
	}
	flavor := snap.Flavors[0]
	if flavor.Signature.UserAgent != "claude-cli/2.1.123 (external, sdk-cli)" {
		t.Fatalf("user agent=%q", flavor.Signature.UserAgent)
	}
	if flavor.BillingAttestation != "abc123def" {
		t.Fatalf("billing attestation=%q want abc123def", flavor.BillingAttestation)
	}
	if updatedAt, _ := store.CurrentBaselineUpdatedAt(context.Background(), "claude-code"); updatedAt == 0 {
		t.Fatalf("updated_at=0 want non-zero after baseline write")
	}
}

func TestRefreshBaselineNoChangeDoesNotRewrite(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	proxy := proxyWithStore(t, store)
	seedClaudeShape(t, proxy, cfg, claudeSystemBillingBody(t, "abc123def"))
	waitForUpstreamShapes(t, store, "claude-code", 1)

	opts := BaselineRefreshOptions{Upstream: "claude-code", IncludeUA: []string{"claude-cli"}}
	if _, err := RefreshBaseline(context.Background(), store, opts); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	firstUpdatedAt, _ := store.CurrentBaselineUpdatedAt(context.Background(), "claude-code")

	// A second refresh against the same corpus must not diverge or rewrite.
	outcome, err := RefreshBaseline(context.Background(), store, opts)
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if outcome.Updated {
		t.Fatalf("Updated=true want false on identical corpus")
	}
	secondUpdatedAt, _ := store.CurrentBaselineUpdatedAt(context.Background(), "claude-code")
	if firstUpdatedAt != secondUpdatedAt {
		t.Fatalf("baseline updated_at changed (%d -> %d) with no drift", firstUpdatedAt, secondUpdatedAt)
	}
}

func TestRefreshBaselineRecordsDiffMatrixOnChange(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	proxy := proxyWithStore(t, store)

	// First baseline: one shape with a constant anthropic-version header.
	headerA := http.Header{}
	headerA.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	headerA.Set("Anthropic-Version", "2023-06-01")
	bodyKeys := claudeSystemBillingBody(t, "cch-a")
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      headerA,
		body:        bodyKeys,
	})
	waitForUpstreamShapes(t, store, "claude-code", 1)
	if _, err := RefreshBaseline(context.Background(), store, BaselineRefreshOptions{Upstream: "claude-code", IncludeUA: []string{"claude-cli"}}); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	// A second shape with the same UA / beta / body keys (same flavor slug) but
	// a different anthropic-version value. The candidate now observes two values
	// for that header within the same flavor, so the constant header in the
	// baseline diverges into an enum: a per-flavor header difference.
	headerB := http.Header{}
	headerB.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	headerB.Set("Anthropic-Version", "2024-10-22")
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      headerB,
		body:        bodyKeys,
	})
	waitForUpstreamShapes(t, store, "claude-code", 2)

	outcome, err := RefreshBaseline(context.Background(), store, BaselineRefreshOptions{Upstream: "claude-code", IncludeUA: []string{"claude-cli"}})
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if !outcome.Diverged {
		t.Fatalf("Diverged=false want true when a header value diverges")
	}
	if !outcome.Updated {
		t.Fatalf("Updated=false want true on divergence")
	}
	diffs, err := store.BaselineDiffs(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("BaselineDiffs: %v", err)
	}
	if len(diffs) == 0 {
		t.Fatalf("baseline_diffs empty; want at least one recorded difference")
	}
}

func TestRefreshBaselineRequiresUpstreamAndStore(t *testing.T) {
	store := openTestCaptureStore(t)
	if _, err := RefreshBaseline(context.Background(), store, BaselineRefreshOptions{Upstream: ""}); err == nil {
		t.Fatal("want error when upstream is empty")
	}
	if _, err := RefreshBaseline(context.Background(), nil, BaselineRefreshOptions{Upstream: "claude-code"}); err == nil {
		t.Fatal("want error when store is nil")
	}
}
