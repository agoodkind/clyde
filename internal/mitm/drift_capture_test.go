package mitm

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm/capture"
)

type claudeBillingBodyFixture struct {
	Model    string                 `json:"model"`
	System   []claudeContentBlock   `json:"system"`
	Messages []claudeMessageFixture `json:"messages"`
}

type claudeContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeMessageFixture struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// claudeSystemBillingBody builds a minimal claude messages body whose first
// system block carries the billing header line with the supplied cch token.
func claudeSystemBillingBody(t *testing.T, cch string) []byte {
	t.Helper()
	body := newClaudeBillingBodyFixture(cch)
	body.Model = "claude-sonnet"
	return mustClaudeBillingBody(t, body)
}

func claudeFeatureBillingBody(t *testing.T, cch string) []byte {
	t.Helper()
	body := newClaudeBillingBodyFixture(cch)
	body.Model = "claude-fable-4-20250514"
	return mustClaudeBillingBody(t, body)
}

func newClaudeBillingBodyFixture(cch string) claudeBillingBodyFixture {
	return claudeBillingBodyFixture{
		Model: "claude-sonnet",
		System: []claudeContentBlock{
			{
				Type: "text",
				Text: "x-anthropic-billing-header: cc_version=2.1.123.fp; cc_entrypoint=cli; cch=" + cch + ";",
			},
			{Type: "text", Text: "You are a helpful assistant."},
		},
		Messages: []claudeMessageFixture{{Role: "user", Content: "hi"}},
	}
}

func mustClaudeBillingBody(t *testing.T, body claudeBillingBodyFixture) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal claude body: %v", err)
	}
	return raw
}

func TestRecordDriftCaptureWritesShapeWithMaskedSecretsAndVerbatimIdentity(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	header := http.Header{}
	header.Set("Authorization", "Bearer super-secret-token")
	header.Set("X-Api-Key", "sk-ant-secret")
	header.Set("Cookie", "session=secret")
	header.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	header.Set("Anthropic-Beta", "oauth-2025-04-20,claude-code-20250219")
	header.Set("Anthropic-Version", "2023-06-01")
	header.Set("X-Stainless-Lang", "js")
	header.Set("X-App", "cli")

	proxy := proxyWithStore(t, store)
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      header,
		body:        claudeSystemBillingBody(t, "abc12345"),
	})

	shapes := waitForUpstreamShapes(t, store, string(driftUpstreamClaudeCode), 1)
	if len(shapes) != 1 {
		t.Fatalf("shape count=%d want 1", len(shapes))
	}
	shape := shapes[0]
	if shape.Method != http.MethodPost || shape.Path != "/v1/messages" {
		t.Fatalf("method/path=%q/%q", shape.Method, shape.Path)
	}
	if shape.URL != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("url=%q", shape.URL)
	}
	for _, secret := range []string{"authorization", "x-api-key", "cookie"} {
		if shape.RequestHeaders[secret] != driftRedactionToken {
			t.Fatalf("header %q=%q want masked %q", secret, shape.RequestHeaders[secret], driftRedactionToken)
		}
	}
	if shape.RequestHeaders["user-agent"] != "claude-cli/2.1.123 (external, sdk-cli)" {
		t.Fatalf("user-agent not verbatim: %q", shape.RequestHeaders["user-agent"])
	}
	if shape.RequestHeaders["anthropic-beta"] != "oauth-2025-04-20,claude-code-20250219" {
		t.Fatalf("anthropic-beta not verbatim: %q", shape.RequestHeaders["anthropic-beta"])
	}
	if shape.RequestHeaders["x-stainless-lang"] != "js" {
		t.Fatalf("x-stainless-lang not verbatim: %q", shape.RequestHeaders["x-stainless-lang"])
	}
	if shape.BillingAttestation != "abc12345" {
		t.Fatalf("billing attestation=%q want abc12345", shape.BillingAttestation)
	}
	var summary captureBodySummary
	if err := json.Unmarshal(shape.RequestBody, &summary); err != nil {
		t.Fatalf("unmarshal body summary: %v", err)
	}
	for _, want := range []string{"messages", "model", "system"} {
		if !containsString(summary.Keys, want) {
			t.Fatalf("body keys missing %q: %v", want, summary.Keys)
		}
	}
	if strings.Contains(string(shape.RequestBody), "helpful assistant") {
		t.Fatalf("body summary leaked prompt text: %s", shape.RequestBody)
	}
}

func TestRecordDriftCaptureCodexCapturesAttestationHeaderVerbatim(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	header := http.Header{}
	header.Set("Authorization", "Bearer secret")
	header.Set("Originator", "codex_cli_rs")
	header.Set("Openai-Beta", "responses=experimental")
	header.Set("X-Oai-Attestation", "attest-token-xyz")
	header.Set("X-Codex-Turn-Metadata", "{}")

	proxy := proxyWithStore(t, store)
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "codex",
		method:      http.MethodPost,
		path:        "/v1/responses",
		upstreamURL: "https://api.openai.com/v1/responses",
		header:      header,
		body:        []byte(`{"model":"gpt-5","input":[]}`),
	})

	shapes := waitForUpstreamShapes(t, store, string(driftUpstreamCodexCLI), 1)
	shape := shapes[0]
	if shape.RequestHeaders["authorization"] != driftRedactionToken {
		t.Fatalf("authorization not masked: %q", shape.RequestHeaders["authorization"])
	}
	if shape.RequestHeaders["x-oai-attestation"] != "attest-token-xyz" {
		t.Fatalf("x-oai-attestation not verbatim: %q", shape.RequestHeaders["x-oai-attestation"])
	}
	if shape.RequestHeaders["originator"] != "codex_cli_rs" {
		t.Fatalf("originator not verbatim: %q", shape.RequestHeaders["originator"])
	}
	if shape.BillingAttestation != "" {
		t.Fatalf("codex billing attestation=%q want empty", shape.BillingAttestation)
	}
}

func TestRecordDriftCaptureSkipsWhenDriftDisabled(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: false}}
	proxy := proxyWithStore(t, store)
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      http.Header{},
		body:        []byte(`{"model":"x"}`),
	})
	if got := upstreamShapeRowCount(t, store, string(driftUpstreamClaudeCode)); got != 0 {
		t.Fatalf("shape count=%d want 0 when drift disabled", got)
	}
}

func TestRecordDriftCaptureDedupsIdenticalShapes(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	header := http.Header{}
	header.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	body := claudeSystemBillingBody(t, "abc12345")
	proxy := proxyWithStore(t, store)
	for range 3 {
		proxy.recordDriftCapture(cfg, driftCaptureInput{
			provider:    "claude",
			method:      http.MethodPost,
			path:        "/v1/messages",
			upstreamURL: "https://api.anthropic.com/v1/messages",
			header:      header,
			body:        body,
		})
	}
	shapes := waitForUpstreamShapes(t, store, string(driftUpstreamClaudeCode), 1)
	if len(shapes) != 1 {
		t.Fatalf("shape rows=%d want 1 (dedup)", len(shapes))
	}
	if shapes[0].SeenCount < 1 {
		t.Fatalf("seen_count=%d want >=1", shapes[0].SeenCount)
	}
}

func TestExtractSnapshotFromShapesYieldsFlavorWithAttestation(t *testing.T) {
	store := openTestCaptureStore(t)
	cfg := config.MITMConfig{Drift: config.MITMDriftConfig{Enabled: true}}
	header := http.Header{}
	header.Set("Authorization", "Bearer secret")
	header.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	header.Set("Anthropic-Beta", "oauth-2025-04-20,claude-code-20250219,model-gated-beta-2026-01-01")

	proxy := proxyWithStore(t, store)
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      header,
		body:        claudeFeatureBillingBody(t, "deadbeef"),
	})

	shapes := waitForUpstreamShapes(t, store, string(driftUpstreamClaudeCode), 1)
	snap, err := ExtractSnapshotFromShapes(shapes, SnapshotOptions{
		UpstreamName:               "claude-code",
		UpstreamVersion:            "test",
		IncludeUserAgentSubstrings: []string{"claude-cli"}, MaxBodyDepth: 0, EnumThreshold: 0,
	})
	if err != nil {
		t.Fatalf("ExtractSnapshotFromShapes: %v", err)
	}
	if len(snap.Flavors) != 1 {
		t.Fatalf("flavor count=%d want 1", len(snap.Flavors))
	}
	flavor := snap.Flavors[0]
	if flavor.BillingAttestation != "deadbeef" {
		t.Fatalf("flavor billing attestation=%q want deadbeef", flavor.BillingAttestation)
	}
	wantFeatures := []RequestFeatures{
		{ModelID: "claude-fable-4-20250514"},
	}
	if !reflect.DeepEqual(flavor.FeatureVectors, wantFeatures) {
		t.Fatalf("flavor feature vectors=%+v want %+v", flavor.FeatureVectors, wantFeatures)
	}
	if len(flavor.Headers) == 0 {
		t.Fatalf("flavor headers empty")
	}
	if len(flavor.Body.Fields) == 0 {
		t.Fatalf("flavor body fields empty")
	}
	hasUA := false
	for _, h := range flavor.Headers {
		if h.Name == "user-agent" {
			hasUA = true
		}
	}
	if !hasUA {
		t.Fatalf("flavor headers missing user-agent: %+v", flavor.Headers)
	}
	bodyKeys := map[string]bool{}
	for _, f := range flavor.Body.Fields {
		bodyKeys[f.Name] = true
	}
	for _, want := range []string{"messages", "model", "system"} {
		if !bodyKeys[want] {
			t.Fatalf("body fields missing %q: %v", want, bodyKeys)
		}
	}
}

// TestSnapshotWeightParity is the parity guarantee: one deduped shape with
// seen_count=N yields the same header classification, presence, occurrence
// rate, and record count as N identical raw shapes (seen_count=1 each).
func TestSnapshotWeightParity(t *testing.T) {
	const n = 5
	base := capture.DriftShape{
		Provider:        "claude",
		Upstream:        "claude-code",
		Method:          "POST",
		Path:            "/v1/messages",
		URL:             "https://api.anthropic.com/v1/messages",
		RequestHeaders:  map[string]string{"user-agent": "claude-cli/2.1.0", "anthropic-version": "2023-06-01"},
		RequestBody:     json.RawMessage(`{"body_type":"json_object","keys":["messages","model"]}`),
		RequestFeatures: json.RawMessage(`{"model_id":"claude-sonnet"}`),
	}

	weighted := base
	weighted.SeenCount = n
	weightedSnap, err := ExtractSnapshotFromShapes([]capture.DriftShape{weighted}, SnapshotOptions{UpstreamName: "claude-code"})
	if err != nil {
		t.Fatalf("weighted extract: %v", err)
	}

	expanded := make([]capture.DriftShape, 0, n)
	for range n {
		one := base
		one.SeenCount = 1
		expanded = append(expanded, one)
	}
	expandedSnap, err := ExtractSnapshotFromShapes(expanded, SnapshotOptions{UpstreamName: "claude-code"})
	if err != nil {
		t.Fatalf("expanded extract: %v", err)
	}

	if weightedSnap.Upstream.RecordCount != n || expandedSnap.Upstream.RecordCount != n {
		t.Fatalf("record counts weighted=%d expanded=%d want %d", weightedSnap.Upstream.RecordCount, expandedSnap.Upstream.RecordCount, n)
	}
	if !reflect.DeepEqual(weightedSnap.Flavors, expandedSnap.Flavors) {
		t.Fatalf("weighted shape with seen_count=%d did not produce the same flavor as %d raw shapes\nweighted: %+v\nexpanded: %+v", n, n, weightedSnap.Flavors, expandedSnap.Flavors)
	}
	if len(weightedSnap.Flavors) != 1 {
		t.Fatalf("flavor count=%d want 1", len(weightedSnap.Flavors))
	}
	for _, h := range weightedSnap.Flavors[0].Headers {
		if h.Presence != HeaderPresenceRequired {
			t.Fatalf("header %q presence=%q want required", h.Name, h.Presence)
		}
		if h.OccurrenceRate != 1.0 {
			t.Fatalf("header %q occurrence=%v want 1.0", h.Name, h.OccurrenceRate)
		}
	}
}

func TestSnapshotJSONRoundTripPreservesFeatureVectors(t *testing.T) {
	in := Snapshot{
		Upstream: Upstream{Name: "claude-code", Version: "v1", CapturedAt: "", RecordCount: 1},
		Flavors: []FlavorShape{
			{
				Slug:        "claude-code-interactive",
				Signature:   Signature{UserAgent: "claude-cli/2.1", BetaFingerprint: "", BodyKeys: []string{"model"}},
				RecordCount: 1,
				Methods:     []string{"POST"},
				Paths:       []string{"/v1/messages"},
				Headers: []Header{
					{Name: "user-agent", Classification: HeaderClassConstant, Presence: HeaderPresenceRequired, ObservedValues: []string{"claude-cli/2.1"}, Pattern: "", OccurrenceRate: 1.0},
				},
				Body: Body{BodyType: "json_object", Fields: []Field{{Name: "model", Kind: FieldKindString, Presence: HeaderPresenceRequired, OccurrenceRate: 1.0, SubFields: nil, ItemKind: "", ItemSubFields: nil, SampleValue: ""}}},
				FeatureVectors: []RequestFeatures{
					{ModelID: "claude-fable-4-20250514"},
					{ModelID: "claude-haiku-4-5-20251001"},
				},
				BillingAttestation: "cch-token-9",
			},
		},
	}
	raw, err := marshalSnapshot(in)
	if err != nil {
		t.Fatalf("marshalSnapshot: %v", err)
	}
	out, err := unmarshalSnapshot(raw)
	if err != nil {
		t.Fatalf("unmarshalSnapshot: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip snapshot mismatch\ngot:  %+v\nwant: %+v", out, in)
	}
}
