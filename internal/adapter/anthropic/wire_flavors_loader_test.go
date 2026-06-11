package anthropic

import (
	"errors"
	"testing"

	"goodkind.io/clyde/internal/mitm"
)

// testAncillaryFlavorShape mirrors the captured shape of a claude-cli
// ancillary GET (observed as /mcp-registry/v0/servers in the live
// baseline): a legitimate caller flavor that carries no
// anthropic-version header at all.
func testAncillaryFlavorShape() mitm.FlavorShape {
	return mitm.FlavorShape{
		Slug:        "claude-code-other-10274fff",
		RecordCount: 12,
		Methods:     []string{"GET"},
		Paths:       []string{"/mcp-registry/v0/servers"},
		Signature: mitm.V2Signature{
			UserAgent: "claude-cli/2.1.170 (external, cli)",
			BodyKeys:  []string{"body_type", "mode"},
		},
		Headers: []mitm.V2Header{
			{Name: "user-agent", Classification: mitm.V2HeaderClassConstant, Presence: mitm.V2HeaderPresenceRequired, ObservedValues: []string{"claude-cli/2.1.170 (external, cli)"}, OccurrenceRate: 1.0},
			{Name: "accept", Classification: mitm.V2HeaderClassConstant, Presence: mitm.V2HeaderPresenceRequired, ObservedValues: []string{"application/json, text/plain, */*"}, OccurrenceRate: 1.0},
		},
	}
}

// writeBaselineWithFlavors seeds a v2 baseline holding exactly the given
// flavors through the real writer, mirroring writeTestWireBaseline.
func writeBaselineWithFlavors(t *testing.T, flavors []mitm.FlavorShape) string {
	t.Helper()
	snap := mitm.SnapshotV2{
		Upstream: mitm.V2Upstream{
			Name:        "claude-code",
			Version:     "",
			CapturedAt:  "2026-06-09T17:34:00Z",
			RecordCount: 13,
		},
		Flavors: flavors,
	}
	out, err := mitm.WriteSnapshotV2TOML(snap, t.TempDir())
	if err != nil {
		t.Fatalf("WriteSnapshotV2TOML: %v", err)
	}
	return out
}

// TestLoaderSkipsFlavorsMissingIdentityHeaders locks in that an
// incomplete ancillary flavor (no constant anthropic-version) is
// skipped with a warning instead of failing the whole load. Regression
// lock for the 2026-06-09 outage where claude-cli /mcp-registry GETs
// entering the learned baseline turned every adapter egress request
// into a 503 wire_baseline_unavailable.
func TestLoaderSkipsFlavorsMissingIdentityHeaders(t *testing.T) {
	t.Parallel()
	path := writeBaselineWithFlavors(t, []mitm.FlavorShape{
		testInteractiveFlavorShape(),
		testAncillaryFlavorShape(),
	})

	flavors, err := newWireFlavorsLoader().Load(path)
	if err != nil {
		t.Fatalf("Load: %v (incomplete ancillary flavor must not fail the load)", err)
	}
	if _, ok := flavors["claude-code-other-10274fff"]; ok {
		t.Fatal("incomplete flavor must be skipped, not projected")
	}
	flavor, ok := selectInteractiveFlavor(flavors)
	if !ok {
		t.Fatal("interactive flavor missing from loaded map")
	}
	if flavor.AnthropicVersion == "" {
		t.Fatalf("interactive flavor AnthropicVersion empty; loaded %q", flavor.Slug)
	}
}

func TestLoaderProjectsFeatureVectors(t *testing.T) {
	t.Parallel()
	shape := testInteractiveFlavorShape()
	shape.FeatureVectors = []mitm.RequestFeatures{
		{
			ModelID:                 "claude-opus-4-20250514",
			Context1M:               true,
			ThinkingMode:            mitm.RequestThinkingEnabled,
			StructuredOutputPresent: true,
			ToolsPresent:            true,
		},
		{
			ModelID:                 "claude-haiku-3-5-20241022",
			Context1M:               false,
			ThinkingMode:            mitm.RequestThinkingNone,
			StructuredOutputPresent: false,
			ToolsPresent:            false,
		},
	}
	path := writeBaselineWithFlavors(t, []mitm.FlavorShape{shape})

	flavors, err := newWireFlavorsLoader().Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	flavor, ok := selectInteractiveFlavor(flavors)
	if !ok {
		t.Fatal("interactive flavor missing from loaded map")
	}
	if len(flavor.FeatureVectors) != 2 {
		t.Fatalf("FeatureVectors len = %d, want 2", len(flavor.FeatureVectors))
	}

	first := flavor.FeatureVectors[0]
	if first.ModelID != "claude-opus-4-20250514" {
		t.Fatalf("FeatureVectors[0].ModelID = %q", first.ModelID)
	}
	if !first.Context1M {
		t.Fatal("FeatureVectors[0].Context1M = false, want true")
	}
	if first.ThinkingMode != WireFlavorThinkingEnabled {
		t.Fatalf("FeatureVectors[0].ThinkingMode = %q", first.ThinkingMode)
	}
	if !first.StructuredOutputPresent {
		t.Fatal("FeatureVectors[0].StructuredOutputPresent = false, want true")
	}
	if !first.ToolsPresent {
		t.Fatal("FeatureVectors[0].ToolsPresent = false, want true")
	}

	second := flavor.FeatureVectors[1]
	if second.ModelID != "claude-haiku-3-5-20241022" {
		t.Fatalf("FeatureVectors[1].ModelID = %q", second.ModelID)
	}
	if second.Context1M {
		t.Fatal("FeatureVectors[1].Context1M = true, want false")
	}
	if second.ThinkingMode != WireFlavorThinkingNone {
		t.Fatalf("FeatureVectors[1].ThinkingMode = %q", second.ThinkingMode)
	}
	if second.StructuredOutputPresent {
		t.Fatal("FeatureVectors[1].StructuredOutputPresent = true, want false")
	}
	if second.ToolsPresent {
		t.Fatal("FeatureVectors[1].ToolsPresent = true, want false")
	}
}

func TestLoaderProjectsMissingFeatureVectorsAsZeroValue(t *testing.T) {
	t.Parallel()
	path := writeBaselineWithFlavors(t, []mitm.FlavorShape{testInteractiveFlavorShape()})

	flavors, err := newWireFlavorsLoader().Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	flavor, ok := selectInteractiveFlavor(flavors)
	if !ok {
		t.Fatal("interactive flavor missing from loaded map")
	}
	if flavor.FeatureVectors != nil {
		t.Fatalf("FeatureVectors = %#v, want nil zero value", flavor.FeatureVectors)
	}
}

// TestLoaderRejectsBaselineWithNoUsableFlavor locks in that a baseline
// where every flavor is incomplete still fails with ErrBaselineInvalid:
// the skip is per-flavor tolerance, not a silent empty-identity path.
func TestLoaderRejectsBaselineWithNoUsableFlavor(t *testing.T) {
	t.Parallel()
	path := writeBaselineWithFlavors(t, []mitm.FlavorShape{testAncillaryFlavorShape()})

	_, err := newWireFlavorsLoader().Load(path)
	if !errors.Is(err, ErrBaselineInvalid) {
		t.Fatalf("Load err = %v, want ErrBaselineInvalid", err)
	}
}
