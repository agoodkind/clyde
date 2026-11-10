package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// TestOutboundHeadersMatchClaudeCLIInteractiveFlavor asserts the
// Anthropic client emits headers that byte-match the captured
// claude-cli interactive flavor loaded at runtime from the daemon-owned
// MITM baseline. CLYDE-124 requires this for parity with the official
// CLI; drift would push our requests onto a different identity and
// degrade quality on Cursor's Anthropic OAuth bucket.
//
// The flavor is no longer compiled in: the test seeds a v2 baseline on
// disk and the client projects it through [WireFlavorsLoader], the same
// path production uses. When the real reference is re-seeded and a
// header genuinely changed upstream, this test fails until the override
// values in the test cfg or the runtime-only branches in
// freeIdentityHeaders are updated to match.
func TestOutboundHeadersMatchClaudeCLIInteractiveFlavor(t *testing.T) {
	t.Parallel()

	var captured atomic.Pointer[http.Header]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Clone()
		captured.Store(&hdr)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	baselineStore := seedTestWireBaseline(t)
	hc := &http.Client{Transport: &rewriteMessagesHost{serverURL: srvURL}}
	cli := &Client{
		http:         hc,
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		// Empty cfg.* override fields force the flavor-driven defaults to
		// be used. Only the fields Send requires before header build are
		// set, plus the capture-store wire baseline.
		cfg: Config{
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			CaptureStore:          baselineStore,
		},
	}

	req := Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	}
	req.FeatureVector = testWireFlavorFeatureVector(req.Model)
	_, _, err = cli.StreamEvents(context.Background(), req, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := captured.Load()
	if got == nil {
		t.Fatal("server did not capture any request")
	}
	loaded, err := cli.flavorLoader.Load(context.Background(), baselineStore)
	if err != nil {
		t.Fatalf("load baseline flavors: %v", err)
	}
	flavor, err := selectInteractiveFlavor(loaded, req.FeatureVector)
	if err != nil {
		t.Fatalf("selectInteractiveFlavor: %v", err)
	}

	if v := got.Get("User-Agent"); v != flavor.UserAgent {
		t.Errorf("User-Agent = %q, want %q", v, flavor.UserAgent)
	}
	if v := got.Get("anthropic-beta"); v != flavor.AnthropicBeta {
		t.Errorf("anthropic-beta = %q, want %q", v, flavor.AnthropicBeta)
	}
	if v := got.Get("anthropic-version"); v != flavor.AnthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", v, flavor.AnthropicVersion)
	}

	for _, h := range flavor.StaticHeaders {
		want := h.Value
		// X-Stainless-Timeout is computed from the http.Client timeout,
		// not the captured value. Skip its strict equality check here;
		// the runtime branch in freeIdentityHeaders is exercised
		// implicitly by the header simply being present.
		if strings.EqualFold(h.Name, "X-Stainless-Timeout") {
			if got.Get(h.Name) == "" {
				t.Errorf("%s missing on outbound", h.Name)
			}
			continue
		}
		if v := got.Get(h.Name); v != want {
			t.Errorf("%s = %q, want %q (from WireFlavor.StaticHeaders)", h.Name, v, want)
		}
	}

	// Per-process session id must be set even though it is not in the
	// flavor (fingerprint is inherently per-process).
	if got.Get("X-Claude-Code-Session-Id") == "" {
		t.Error("X-Claude-Code-Session-Id missing on outbound")
	}
}

func TestFeatureAwareFlavorSelectionMatchesModelBeforeFlags(t *testing.T) {
	t.Parallel()

	loaded, err := newWireFlavorsLoader().Load(context.Background(), seedTestWireBaseline(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	flavor, err := selectInteractiveFlavor(loaded, testWireFlavorFeatureVector(testSonnetModel))
	if err != nil {
		t.Fatalf("selectInteractiveFlavor: %v", err)
	}
	if flavor.Slug != testSonnetFlavorSlug {
		t.Fatalf("selected slug = %q, want %q", flavor.Slug, testSonnetFlavorSlug)
	}
	if flavor.Slug == testFableStandardFlavorSlug || flavor.Slug == testFable1MFlavorSlug {
		t.Fatalf("sonnet request selected fable flavor %q", flavor.Slug)
	}
}

// TestOutboundBetaHeaderStripsConfiguredWireFlags asserts the outbound
// anthropic-beta header drops exactly the tokens named in
// cfg.StripWireFlags, case-insensitively, leaving every other learned flag
// in order. The suppression list is operator config, so the test uses
// generic fixture tokens rather than any provider's real flag vocabulary.
func TestOutboundBetaHeaderStripsConfiguredWireFlags(t *testing.T) {
	t.Parallel()

	cli := &Client{
		http:         &http.Client{},
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "override-beta-flag",
			StripWireFlags:        []string{"strip-me-a", "strip-me-b"},
		},
	}
	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cli.cfg.MessagesURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	flavor := WireFlavor{
		Slug:             "test-wire-flag-strip",
		UserAgent:        "claude-cli/2.1.123 (external, sdk-cli)",
		AnthropicVersion: "2023-06-01",
		AnthropicBeta:    "keep-a,Strip-Me-A,keep-b,Strip-Me-B,keep-c",
	}
	req := Request{
		Model: "claude-test",
	}

	cli.applyMessagesHeaders(httpReq, req, "tok", flavor)

	want := "keep-a,keep-b,keep-c"
	if got := httpReq.Header.Get("Anthropic-Beta"); got != want {
		t.Fatalf("Anthropic-Beta = %q, want %q", got, want)
	}
}

// TestOutboundHeadersAllowNonBetaConfigOverride confirms that
// non-beta cfg.ClientIdentity.* overrides still take effect when set.
func TestOutboundHeadersAllowNonBetaConfigOverride(t *testing.T) {
	t.Parallel()

	var captured atomic.Pointer[http.Header]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Clone()
		captured.Store(&hdr)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	hc := &http.Client{Transport: &rewriteMessagesHost{serverURL: srvURL}}
	cli := &Client{
		http:         hc,
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:             "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion:   "2023-06-01",
			BetaHeader:              "override-beta-flag",
			UserAgent:               "override-cli/9.9.9 (test)",
			StainlessPackageVersion: "override-pkg",
			StainlessRuntime:        "override-runtime",
			StainlessRuntimeVersion: "override-runtime-version",
			CaptureStore:            seedTestWireBaseline(t),
		},
	}

	req := Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	}
	req.FeatureVector = testWireFlavorFeatureVector(req.Model)
	_, _, err = cli.StreamEvents(context.Background(), req, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := captured.Load()
	if got == nil {
		t.Fatal("server did not capture any request")
	}
	wants := map[string]string{
		"User-Agent":                  "override-cli/9.9.9 (test)",
		"X-Stainless-Package-Version": "override-pkg",
		"X-Stainless-Runtime":         "override-runtime",
		"X-Stainless-Runtime-Version": "override-runtime-version",
	}
	for name, want := range wants {
		if v := got.Get(name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	if v := got.Get("anthropic-beta"); v != testStandardBetaHeader {
		t.Errorf("anthropic-beta = %q, want selected flavor beta %q", v, testStandardBetaHeader)
	}
}
