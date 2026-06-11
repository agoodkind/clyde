package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSubstituteBillingAttestation(t *testing.T) {
	t.Parallel()
	const placeholderLine = "x-anthropic-billing-header: cc_version=2.1.123.abc; cc_entrypoint=sdk-cli; cch=00000;"
	tests := []struct {
		name string
		line string
		cch  string
		want string
	}{
		{
			name: "swaps_cch_in_billing_line",
			line: placeholderLine,
			cch:  "deadbeef",
			want: "x-anthropic-billing-header: cc_version=2.1.123.abc; cc_entrypoint=sdk-cli; cch=deadbeef;",
		},
		{
			name: "empty_cch_keeps_placeholder",
			line: placeholderLine,
			cch:  "",
			want: placeholderLine,
		},
		{
			name: "whitespace_cch_keeps_placeholder",
			line: placeholderLine,
			cch:  "   ",
			want: placeholderLine,
		},
		{
			name: "non_billing_line_untouched",
			line: "some other system text with cch= in it",
			cch:  "deadbeef",
			want: "some other system text with cch= in it",
		},
		{
			name: "appends_cch_when_marker_missing",
			line: "x-anthropic-billing-header: cc_version=2.1.123.abc; cc_entrypoint=sdk-cli;",
			cch:  "deadbeef",
			want: "x-anthropic-billing-header: cc_version=2.1.123.abc; cc_entrypoint=sdk-cli; cch=deadbeef;",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SubstituteBillingAttestation(tt.line, tt.cch); got != tt.want {
				t.Fatalf("SubstituteBillingAttestation(%q, %q) = %q want %q", tt.line, tt.cch, got, tt.want)
			}
		})
	}
}

func TestApplyBillingAttestationOnlyTouchesBillingBlock(t *testing.T) {
	t.Parallel()
	blocks := []SystemBlock{
		{Type: "text", Text: "x-anthropic-billing-header: cc_version=2.1.123.abc; cc_entrypoint=sdk-cli; cch=00000;"},
		{Type: "text", Text: "prefix block (must not change)"},
		{Type: "text", Text: "caller system (must not change)"},
	}
	applyBillingAttestation(blocks, "deadbeef")
	if !strings.Contains(blocks[0].Text, "cch=deadbeef;") {
		t.Fatalf("billing block cch not substituted: %q", blocks[0].Text)
	}
	if blocks[1].Text != "prefix block (must not change)" {
		t.Fatalf("prefix block mutated: %q", blocks[1].Text)
	}
	if blocks[2].Text != "caller system (must not change)" {
		t.Fatalf("caller block mutated: %q", blocks[2].Text)
	}
}

func TestApplyBillingAttestationEmptyAttestationKeepsPlaceholder(t *testing.T) {
	t.Parallel()
	const placeholder = "x-anthropic-billing-header: cc_version=2.1.123.abc; cc_entrypoint=sdk-cli; cch=00000;"
	blocks := []SystemBlock{{Type: "text", Text: placeholder}}
	applyBillingAttestation(blocks, "")
	if blocks[0].Text != placeholder {
		t.Fatalf("placeholder cch must survive empty attestation, got %q", blocks[0].Text)
	}
}

// TestCheckBodyShapeFiresOnMissingRequiredField asserts the wire-shape
// diagnostic emits the body_shape_mismatch event on the request concern
// when the outbound body is missing a learned-required field.
func TestCheckBodyShapeFiresOnMissingRequiredField(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	flavor := WireFlavor{BodyFieldsRequired: []string{"max_tokens", "messages", "model"}}
	// Body intentionally omits "model" so the required-field check fires.
	body := []byte(`{"max_tokens":10,"messages":[],"stream":false}`)
	checkBodyShape(context.Background(), "claude-test", body, flavor)

	out := buf.String()
	if !strings.Contains(out, "anthropic.messages.body_shape_mismatch") {
		t.Fatalf("expected body_shape_mismatch event, got: %s", out)
	}
	if !strings.Contains(out, `"missing":["model"]`) {
		t.Fatalf("expected missing=[model], got: %s", out)
	}
}

func TestCheckBodyShapeSilentWhenAllRequiredPresent(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	flavor := WireFlavor{BodyFieldsRequired: []string{"max_tokens", "messages", "model"}}
	body := []byte(`{"model":"claude-test","messages":[],"max_tokens":10,"stream":false}`)
	checkBodyShape(context.Background(), "claude-test", body, flavor)

	if strings.Contains(buf.String(), "body_shape_mismatch") {
		t.Fatalf("no mismatch event expected when all required fields present, got: %s", buf.String())
	}
}

// TestAnthropicVersionPrefersFlavor asserts the outbound Anthropic-Version
// header is sourced from the learned flavor, even when the configured
// OAuthAnthropicVersion differs.
func TestAnthropicVersionPrefersFlavor(t *testing.T) {
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
			MessagesURL: "https://REDACTED-UPSTREAM/v1/messages",
			// Configured value differs from the flavor's 2023-06-01 so a
			// regression that reads cfg instead of the flavor is visible.
			OAuthAnthropicVersion: "1999-01-01",
			WireBaselinePath:      writeTestWireBaseline(t),
		},
	}

	_, _, err = cli.StreamEvents(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	}, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	got := captured.Load()
	if got == nil {
		t.Fatal("server captured no request")
	}
	if v := got.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q, want flavor value 2023-06-01 (not cfg 1999-01-01)", v)
	}
}

func TestFeatureAwareFlavorSelectionMatchesContextTier(t *testing.T) {
	t.Parallel()

	loaded, err := newWireFlavorsLoader().Load(writeTestWireBaseline(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		name        string
		vector      WireFlavorFeatureVector
		wantContext bool
	}{
		{
			name:        "fable_1m",
			vector:      testWireFlavorFeatureVector(testFableModel, true),
			wantContext: true,
		},
		{
			name:        "opus_1m",
			vector:      testWireFlavorFeatureVector(testOpusModel, true),
			wantContext: true,
		},
		{
			name:        "fable_200k",
			vector:      testWireFlavorFeatureVector(testFableModel, false),
			wantContext: false,
		},
		{
			name:        "sonnet_200k",
			vector:      testWireFlavorFeatureVector(testSonnetModel, false),
			wantContext: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			flavor, err := selectInteractiveFlavor(loaded, tc.vector)
			if err != nil {
				t.Fatalf("selectInteractiveFlavor: %v", err)
			}
			gotContext := betaFlagsContain(flavor, testContext1MBeta)
			if gotContext != tc.wantContext {
				t.Fatalf("selected %q context-1m=%v, want %v", flavor.Slug, gotContext, tc.wantContext)
			}
		})
	}
}

func betaFlagsContain(flavor WireFlavor, flag string) bool {
	for _, betaFlag := range flavor.BetaFlags {
		if betaFlag == flag {
			return true
		}
	}
	return false
}

// TestAnthropicVersionFallsBackToConfig exercises the cfg fallback branch
// directly: when a flavor carries no Anthropic-Version (a state the
// loader skips during projection but the header builder still guards
// against), the configured OAuthAnthropicVersion is used.
func TestAnthropicVersionFallsBackToConfig(t *testing.T) {
	t.Parallel()
	cli := &Client{
		http:         &http.Client{},
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
		},
	}
	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cli.cfg.MessagesURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	flavor := WireFlavor{
		Slug:             "test",
		UserAgent:        "claude-cli/0 (test)",
		AnthropicVersion: "",
	}
	cli.applyMessagesHeaders(httpReq, Request{Model: "claude-test"}, "tok", flavor)
	if v := httpReq.Header.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q, want cfg fallback 2023-06-01", v)
	}
}

// TestBillingAttestationReplayedIntoOutboundBody asserts that a non-empty
// captured attestation is substituted into the attribution block's cch on
// the outbound body, replacing the locally generated placeholder.
func TestBillingAttestationReplayedIntoOutboundBody(t *testing.T) {
	t.Parallel()

	var capturedBody atomic.Pointer[[]byte]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readAll(t, r)
		capturedBody.Store(&b)
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
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			WireBaselinePath:      writeTestWireBaselineWithAttestation(t, "captured-cch-7777"),
		},
	}

	billing := "x-anthropic-billing-header: cc_version=2.1.123.abc; cc_entrypoint=sdk-cli; cch=00000;"
	_, _, err = cli.StreamEvents(context.Background(), Request{
		Model:        "claude-test",
		SystemBlocks: []SystemBlock{{Type: "text", Text: billing}},
		Messages:     []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens:    10,
	}, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	bodyPtr := capturedBody.Load()
	if bodyPtr == nil {
		t.Fatal("server captured no body")
	}
	body := string(*bodyPtr)
	if !strings.Contains(body, "cch=captured-cch-7777;") {
		t.Fatalf("captured attestation not substituted into outbound body: %s", body)
	}
	if strings.Contains(body, "cch=00000;") {
		t.Fatalf("placeholder cch leaked onto the wire: %s", body)
	}
}

// TestBillingAttestationEmptyKeepsPlaceholderOnWire asserts that with an
// empty captured attestation the locally generated placeholder cch stays
// on the outbound body.
func TestBillingAttestationEmptyKeepsPlaceholderOnWire(t *testing.T) {
	t.Parallel()

	var capturedBody atomic.Pointer[[]byte]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readAll(t, r)
		capturedBody.Store(&b)
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
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			WireBaselinePath:      writeTestWireBaseline(t),
		},
	}

	billing := "x-anthropic-billing-header: cc_version=2.1.123.abc; cc_entrypoint=sdk-cli; cch=abc99;"
	_, _, err = cli.StreamEvents(context.Background(), Request{
		Model:        "claude-test",
		SystemBlocks: []SystemBlock{{Type: "text", Text: billing}},
		Messages:     []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens:    10,
	}, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	bodyPtr := capturedBody.Load()
	if bodyPtr == nil {
		t.Fatal("server captured no body")
	}
	if !strings.Contains(string(*bodyPtr), "cch=abc99;") {
		t.Fatalf("placeholder cch should survive an empty attestation: %s", string(*bodyPtr))
	}
}

// TestOutboundBodyFieldSetMatchesLearnedShape asserts the outbound body
// top-level field order mirrors the learned claude-code request: model,
// system, messages, max_tokens, stream come first in that declaration
// order. The captured Cursor content (messages, system text, model,
// max_tokens) is preserved, not discarded.
func TestOutboundBodyFieldSetMatchesLearnedShape(t *testing.T) {
	t.Parallel()

	var capturedBody atomic.Pointer[[]byte]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readAll(t, r)
		capturedBody.Store(&b)
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
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			WireBaselinePath:      writeTestWireBaseline(t),
		},
	}

	_, _, err = cli.StreamEvents(context.Background(), Request{
		Model:        "claude-test",
		SystemBlocks: []SystemBlock{{Type: "text", Text: "system text"}},
		Messages:     []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}}},
		MaxTokens:    10,
	}, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	bodyPtr := capturedBody.Load()
	if bodyPtr == nil {
		t.Fatal("server captured no body")
	}
	keys := decodedTopLevelKeyOrder(t, *bodyPtr)
	// The Request struct declaration order mirrors claude-code:
	// system (spliced first), then model, messages, max_tokens, stream.
	leadingWant := []string{"system", "model", "messages", "max_tokens", "stream"}
	if len(keys) < len(leadingWant) {
		t.Fatalf("outbound body has too few keys %v", keys)
	}
	for i, want := range leadingWant {
		if keys[i] != want {
			t.Fatalf("outbound body key[%d] = %q want %q (order %v)", i, keys[i], want, keys)
		}
	}
	// Content preservation: the learned-required fields are present and
	// the caller content is not dropped.
	body := string(*bodyPtr)
	if !strings.Contains(body, `"messages"`) || !strings.Contains(body, "hello") {
		t.Fatalf("caller messages content dropped: %s", body)
	}
	if !strings.Contains(body, "system text") {
		t.Fatalf("caller system content dropped: %s", body)
	}
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return b
}

// decodedTopLevelKeyOrder returns the top-level JSON keys of body in the
// order they appear on the wire.
func decodedTopLevelKeyOrder(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	if _, err := dec.Token(); err != nil {
		t.Fatalf("read open token: %v", err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key token: %v", err)
		}
		key, ok := tok.(string)
		if !ok {
			t.Fatalf("key token is not a string: %v", tok)
		}
		keys = append(keys, key)
		skipOneJSONValue(t, dec)
	}
	return keys
}

func skipOneJSONValue(t *testing.T, dec *json.Decoder) {
	t.Helper()
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("read value token: %v", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return
	}
	depth := 1
	for depth > 0 {
		next, err := dec.Token()
		if err != nil {
			t.Fatalf("read nested token: %v", err)
		}
		if d, ok := next.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
}
