package anthropic

import (
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/mitm"
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
	testModelGatedBeta          = "model-gated-beta-2026-01-01"
	testModelGatedBetaHeader    = testStandardBetaHeader + "," + testModelGatedBeta
	testDefaultFlavorSlug       = "claude-code-interactive-default-17c1f069"
	testSonnetFlavorSlug        = "claude-code-interactive-sonnet-200k"
	testFableStandardFlavorSlug = "claude-code-interactive-fable-200k"
	testFable1MFlavorSlug       = "claude-code-interactive-fable-1m"
	testOpus1MFlavorSlug        = "claude-code-interactive-opus-1m"
)

// writeTestWireBaseline writes a minimal but realistic v2 MITM baseline
// to a temp dir and returns its path. The baseline carries learned
// interactive flavors for several model and context combinations plus
// one probe flavor, so tests exercise feature-aware selection. There
// is no committed default TOML: every test seeds its own baseline
// through the real
// [mitm.WriteSnapshotV2TOML] writer and loads it back through
// [WireFlavorsLoader], the same path production uses.
func writeTestWireBaseline(t *testing.T) string {
	t.Helper()
	return writeTestWireBaselineWithAttestation(t, "")
}

// writeTestWireBaselineWithAttestation seeds the same baseline as
// [writeTestWireBaseline] but stamps the interactive flavor with the
// given captured billing attestation (the `cch` token). An empty cch
// leaves the flavor without an attestation so callers can exercise both
// the substitution and placeholder-fallback egress branches.
func writeTestWireBaselineWithAttestation(t *testing.T, cch string) string {
	t.Helper()
	dir := t.TempDir()
	defaultFlavor := testInteractiveFlavorShapeForModel(testDefaultFlavorSlug, testDefaultModel, testStandardBetaHeader)
	defaultFlavor.BillingAttestation = cch
	snap := mitm.SnapshotV2{
		Upstream: mitm.V2Upstream{
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
	out, err := mitm.WriteSnapshotV2TOML(snap, dir)
	if err != nil {
		t.Fatalf("WriteSnapshotV2TOML: %v", err)
	}
	if filepath.Base(out) != "reference-v2.toml" {
		t.Fatalf("unexpected baseline filename %q", out)
	}
	return out
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
		Signature: mitm.V2Signature{
			UserAgent:       "claude-cli/2.1.123 (external, sdk-cli)",
			BetaFingerprint: betaHeader,
			BodyKeys:        []string{"max_tokens", "messages", "metadata", "model", "stream", "system"},
		},
		Headers: testInteractiveFlavorHeadersWithBeta(betaHeader),
		Body: mitm.V2Body{
			BodyType: "object",
			Fields: []mitm.V2Field{
				{Name: "max_tokens", Kind: mitm.V2FieldKindNumber, Presence: mitm.V2HeaderPresenceRequired, OccurrenceRate: 1.0},
				{Name: "messages", Kind: mitm.V2FieldKindArray, Presence: mitm.V2HeaderPresenceRequired, OccurrenceRate: 1.0},
				{Name: "model", Kind: mitm.V2FieldKindString, Presence: mitm.V2HeaderPresenceRequired, OccurrenceRate: 1.0},
				{Name: "stream", Kind: mitm.V2FieldKindBool, Presence: mitm.V2HeaderPresenceOptional, OccurrenceRate: 0.5},
			},
		},
	}
}

func testInteractiveFlavorHeaders() []mitm.V2Header {
	return testInteractiveFlavorHeadersWithBeta(testStandardBetaHeader)
}

func testInteractiveFlavorHeadersWithBeta(betaHeader string) []mitm.V2Header {
	constant := func(name, value string) mitm.V2Header {
		return mitm.V2Header{
			Name:           name,
			Classification: mitm.V2HeaderClassConstant,
			Presence:       mitm.V2HeaderPresenceRequired,
			ObservedValues: []string{value},
			OccurrenceRate: 1.0,
		}
	}
	return []mitm.V2Header{
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
		Signature: mitm.V2Signature{
			UserAgent: "claude-cli/2.1.123 (external, sdk-cli)",
			BodyKeys:  []string{"max_tokens", "messages", "model", "stream"},
		},
		Headers: []mitm.V2Header{
			{Name: "user-agent", Classification: mitm.V2HeaderClassConstant, Presence: mitm.V2HeaderPresenceRequired, ObservedValues: []string{"claude-cli/2.1.123 (external, sdk-cli)"}, OccurrenceRate: 1.0},
			{Name: "anthropic-version", Classification: mitm.V2HeaderClassConstant, Presence: mitm.V2HeaderPresenceRequired, ObservedValues: []string{"2023-06-01"}, OccurrenceRate: 1.0},
			{Name: "anthropic-beta", Classification: mitm.V2HeaderClassConstant, Presence: mitm.V2HeaderPresenceRequired, ObservedValues: []string{"oauth-2025-04-20"}, OccurrenceRate: 1.0},
		},
	}
}
