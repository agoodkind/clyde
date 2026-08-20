package anthropic

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitm/capture"
)

const (
	testDefaultModel       = "claude-test"
	testSonnetModel        = "claude-sonnet-4-5-20250929"
	testFableModel         = "claude-fable-4-20250514"
	testOpusModel          = "claude-opus-4-20250514"
	testStandardBetaHeader = "claude-code-20250219,oauth-2025-04-20"
	// testModelGatedBeta is a generic capability token the test baseline
	// attaches only to the fable and opus flavors, standing in for any
	// per-model beta the learned baseline carries. It is deliberately not a
	// real provider flag, so no vendor wire vocabulary lives in the tests.
	testModelGatedBeta = "model-gated-beta-2026-01-01"
	// testStaleAttestationToken stands in for a captured claude-cli
	// attestation token. Clyde cannot mint one, and replaying a stale value
	// makes the upstream withhold thinking plaintext, so the projection must
	// drop it rather than carry it onto outbound requests.
	testStaleAttestationToken   = "stale-attestation-token"
	testModelGatedBetaHeader    = testStandardBetaHeader + "," + testModelGatedBeta
	testDefaultFlavorSlug       = "claude-code-interactive-default-17c1f069"
	testSonnetFlavorSlug        = "claude-code-interactive-sonnet-200k"
	testFableStandardFlavorSlug = "claude-code-interactive-fable-200k"
	testFable1MFlavorSlug       = "claude-code-interactive-fable-1m"
	testOpus1MFlavorSlug        = "claude-code-interactive-opus-1m"
)

// seedBaselineStore opens a real capture store, writes the given snapshot as
// the current claude-code baseline through PutBaseline, and returns the store.
// The adapter reads the baseline back through the same store the daemon uses in
// production.
func seedBaselineStore(t *testing.T, snap mitm.Snapshot) *capture.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := store.PutBaseline(context.Background(), capture.BaselineChange{
		Timestamp: time.Unix(1700000000, 0),
		Upstream:  "claude-code",
		Snapshot:  raw,
		Diffs:     nil,
	}); err != nil {
		t.Fatalf("PutBaseline: %v", err)
	}
	return store
}

// seedBaselineIntoStore writes the standard test baseline into an existing
// capture store, for tests that already opened a store (for example to record
// egress) and need it to also serve the wire baseline.
func seedBaselineIntoStore(t *testing.T, store *capture.Store) {
	t.Helper()
	raw, err := json.Marshal(testWireBaselineSnapshot(""))
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := store.PutBaseline(context.Background(), capture.BaselineChange{
		Timestamp: time.Unix(1700000000, 0),
		Upstream:  "claude-code",
		Snapshot:  raw,
		Diffs:     nil,
	}); err != nil {
		t.Fatalf("PutBaseline: %v", err)
	}
}

// seedTestWireBaseline seeds a real capture store with the standard test
// baseline (several learned interactive flavors plus a probe flavor) and
// returns the store. There is no committed default snapshot: every test seeds
// its own baseline through PutBaseline and loads it back through
// [WireFlavorsLoader], the same path production uses.
func seedTestWireBaseline(t *testing.T) *capture.Store {
	t.Helper()
	return seedTestWireBaselineWithAttestation(t, "")
}

// seedTestWireBaselineWithAttestation seeds the same baseline as
// [seedTestWireBaseline] but stamps the interactive flavor with the given
// captured billing attestation (the `cch` token). An empty cch leaves the
// flavor without an attestation so callers can exercise both the substitution
// and placeholder-fallback egress branches.
func seedTestWireBaselineWithAttestation(t *testing.T, cch string) *capture.Store {
	t.Helper()
	return seedBaselineStore(t, testWireBaselineSnapshot(cch))
}

func testWireBaselineSnapshot(cch string) mitm.Snapshot {
	defaultFlavor := testInteractiveFlavorShapeForModel(testDefaultFlavorSlug, testDefaultModel, testStandardBetaHeader)
	defaultFlavor.BillingAttestation = cch
	return mitm.Snapshot{
		Upstream: mitm.Upstream{
			Name:        "claude-code",
			Version:     "",
			CapturedAt:  "2026-04-30T04:30:53Z",
			RecordCount: 8,
		},
		Flavors: []mitm.FlavorShape{
			defaultFlavor,
			testInteractiveFlavorShapeForModel(testSonnetFlavorSlug, testSonnetModel, testStandardBetaHeader),
			testInteractiveFlavorShapeForModel(testFableStandardFlavorSlug, testFableModel, testStandardBetaHeader),
			testInteractiveFlavorShapeForModel(testFable1MFlavorSlug, testFableModel, testModelGatedBetaHeader),
			testInteractiveFlavorShapeForModel(testOpus1MFlavorSlug, testOpusModel, testModelGatedBetaHeader),
			testProbeFlavorShape(),
		},
	}
}

func testInteractiveFlavorShape() mitm.FlavorShape {
	return testInteractiveFlavorShapeWithBeta("claude-code-interactive-17c1f069", testStandardBetaHeader)
}

func testInteractiveFlavorShapeForModel(slug string, model string, betaHeader string) mitm.FlavorShape {
	shape := testInteractiveFlavorShapeWithBeta(slug, betaHeader)
	shape.FeatureVectors = []mitm.RequestFeatures{testRequestFeatures(model)}
	return shape
}

func testInteractiveFlavorShapeWithBeta(slug string, betaHeader string) mitm.FlavorShape {
	return mitm.FlavorShape{
		Slug:        slug,
		RecordCount: 1,
		Methods:     []string{"POST"},
		Paths:       []string{"/v1/messages"},
		Signature: mitm.Signature{
			UserAgent:       "claude-cli/2.1.123 (external, sdk-cli)",
			BetaFingerprint: betaHeader,
			BodyKeys:        []string{"max_tokens", "messages", "metadata", "model", "stream", "system"},
		},
		Headers: testInteractiveFlavorHeadersWithBeta(betaHeader),
		Body: mitm.Body{
			BodyType: "object",
			Fields: []mitm.Field{
				{Name: "max_tokens", Kind: mitm.FieldKindNumber, Presence: mitm.HeaderPresenceRequired, OccurrenceRate: 1.0},
				{Name: "messages", Kind: mitm.FieldKindArray, Presence: mitm.HeaderPresenceRequired, OccurrenceRate: 1.0},
				{Name: "model", Kind: mitm.FieldKindString, Presence: mitm.HeaderPresenceRequired, OccurrenceRate: 1.0},
				{Name: "stream", Kind: mitm.FieldKindBool, Presence: mitm.HeaderPresenceOptional, OccurrenceRate: 0.5},
			},
		},
	}
}

func testInteractiveFlavorHeaders() []mitm.Header {
	return testInteractiveFlavorHeadersWithBeta(testStandardBetaHeader)
}

func testInteractiveFlavorHeadersWithBeta(betaHeader string) []mitm.Header {
	constant := func(name, value string) mitm.Header {
		return mitm.Header{
			Name:           name,
			Classification: mitm.HeaderClassConstant,
			Presence:       mitm.HeaderPresenceRequired,
			ObservedValues: []string{value},
			OccurrenceRate: 1.0,
		}
	}
	return []mitm.Header{
		constant("user-agent", "claude-cli/2.1.123 (external, sdk-cli)"),
		constant("anthropic-version", "2023-06-01"),
		constant("anthropic-beta", betaHeader),
		constant("anthropic-dangerous-direct-browser-access", "true"),
		constant("x-app", "cli"),
		constant("x-stainless-arch", "arm64"),
		constant("x-stainless-lang", "js"),
		constant("x-stainless-os", "MacOS"),
		constant("x-stainless-package-version", "0.81.0"),
		constant("x-stainless-retry-count", "0"),
		constant("x-stainless-runtime", "node"),
		constant("x-stainless-runtime-version", "v24.3.0"),
		constant("x-stainless-timeout", "600"),
		// Secret-bearing and per-session headers are excluded by the
		// loader's projection; include them so the test exercises that
		// exclusion path.
		constant("authorization", "<redacted>"),
		constant("x-claude-code-session-id", "abc-123"),
		constant("x-cc-atis", testStaleAttestationToken),
	}
}

func testRequestFeatures(model string) mitm.RequestFeatures {
	return mitm.RequestFeatures{ModelID: model}
}

func testWireFlavorFeatureVector(model string) WireFlavorFeatureVector {
	return WireFlavorFeatureVector{ModelID: model}
}

func testProbeFlavorShape() mitm.FlavorShape {
	return mitm.FlavorShape{
		Slug:        "claude-code-probe-e5e49e54",
		RecordCount: 1,
		Methods:     []string{"POST"},
		Paths:       []string{"/v1/messages"},
		Signature: mitm.Signature{
			UserAgent: "claude-cli/2.1.123 (external, sdk-cli)",
			BodyKeys:  []string{"max_tokens", "messages", "model", "stream"},
		},
		Headers: []mitm.Header{
			{Name: "user-agent", Classification: mitm.HeaderClassConstant, Presence: mitm.HeaderPresenceRequired, ObservedValues: []string{"claude-cli/2.1.123 (external, sdk-cli)"}, OccurrenceRate: 1.0},
			{Name: "anthropic-version", Classification: mitm.HeaderClassConstant, Presence: mitm.HeaderPresenceRequired, ObservedValues: []string{"2023-06-01"}, OccurrenceRate: 1.0},
			{Name: "anthropic-beta", Classification: mitm.HeaderClassConstant, Presence: mitm.HeaderPresenceRequired, ObservedValues: []string{"oauth-2025-04-20"}, OccurrenceRate: 1.0},
		},
	}
}
