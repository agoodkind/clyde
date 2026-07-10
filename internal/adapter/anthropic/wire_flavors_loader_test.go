package anthropic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitm/capture"
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
		Signature: mitm.Signature{
			UserAgent: "claude-cli/2.1.170 (external, cli)",
			BodyKeys:  []string{"body_type", "mode"},
		},
		Headers: []mitm.Header{
			{Name: "user-agent", Classification: mitm.HeaderClassConstant, Presence: mitm.HeaderPresenceRequired, ObservedValues: []string{"claude-cli/2.1.170 (external, cli)"}, OccurrenceRate: 1.0},
			{Name: "accept", Classification: mitm.HeaderClassConstant, Presence: mitm.HeaderPresenceRequired, ObservedValues: []string{"application/json, text/plain, */*"}, OccurrenceRate: 1.0},
		},
	}
}

// seedStoreWithFlavors seeds a capture store baseline holding exactly the given
// flavors, mirroring seedTestWireBaseline.
func seedStoreWithFlavors(t *testing.T, flavors []mitm.FlavorShape) *capture.Store {
	t.Helper()
	return seedBaselineStore(t, mitm.Snapshot{
		Upstream: mitm.Upstream{
			Name:        "claude-code",
			Version:     "",
			CapturedAt:  "2026-06-09T17:34:00Z",
			RecordCount: 13,
		},
		Flavors: flavors,
	})
}

// TestLoaderSkipsFlavorsMissingIdentityHeaders locks in that an
// incomplete ancillary flavor (no constant anthropic-version) is
// skipped with a warning instead of failing the whole load. Regression
// lock for the 2026-06-09 outage where claude-cli /mcp-registry GETs
// entering the learned baseline turned every adapter egress request
// into a 503 wire_baseline_unavailable.
func TestLoaderSkipsFlavorsMissingIdentityHeaders(t *testing.T) {
	store := seedStoreWithFlavors(t, []mitm.FlavorShape{
		testInteractiveFlavorShape(),
		testAncillaryFlavorShape(),
	})

	flavors, err := newWireFlavorsLoader().Load(context.Background(), store)
	if err != nil {
		t.Fatalf("Load: %v (incomplete ancillary flavor must not fail the load)", err)
	}
	if _, ok := flavors["claude-code-other-10274fff"]; ok {
		t.Fatal("incomplete flavor must be skipped, not projected")
	}
	flavor, ok := flavors["claude-code-interactive-17c1f069"]
	if !ok {
		t.Fatal("interactive flavor missing from loaded map")
	}
	if flavor.AnthropicVersion == "" {
		t.Fatalf("interactive flavor AnthropicVersion empty; loaded %q", flavor.Slug)
	}
}

func TestLoaderProjectsFeatureVectors(t *testing.T) {
	shape := testInteractiveFlavorShape()
	shape.FeatureVectors = []mitm.RequestFeatures{
		{ModelID: "claude-opus-4-20250514"},
		{ModelID: "claude-haiku-3-5-20241022"},
	}
	store := seedStoreWithFlavors(t, []mitm.FlavorShape{shape})

	flavors, err := newWireFlavorsLoader().Load(context.Background(), store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	flavor, err := selectInteractiveFlavor(flavors, WireFlavorFeatureVector{ModelID: "claude-opus-4-20250514"})
	if err != nil {
		t.Fatalf("selectInteractiveFlavor: %v", err)
	}
	if len(flavor.FeatureVectors) != 2 {
		t.Fatalf("FeatureVectors len = %d, want 2", len(flavor.FeatureVectors))
	}
	if got := flavor.FeatureVectors[0].ModelID; got != "claude-opus-4-20250514" {
		t.Fatalf("FeatureVectors[0].ModelID = %q", got)
	}
	if got := flavor.FeatureVectors[1].ModelID; got != "claude-haiku-3-5-20241022" {
		t.Fatalf("FeatureVectors[1].ModelID = %q", got)
	}
}

func TestSelectInteractiveFlavorUsesConfiguredWireProfile(t *testing.T) {
	flavors := map[string]WireFlavor{
		"claude-code-interactive-default": {
			Slug: "claude-code-interactive-default",
			FeatureVectors: []WireFlavorFeatureVector{{
				ModelID: "claude-requested",
			}},
		},
		"learned-exact": {
			Slug: "learned-exact",
		},
	}

	flavor, err := selectInteractiveFlavor(flavors, WireFlavorFeatureVector{
		ModelID:     "claude-requested",
		WireProfile: "learned-exact",
	})
	if err != nil {
		t.Fatalf("selectInteractiveFlavor: %v", err)
	}
	if flavor.Slug != "learned-exact" {
		t.Fatalf("flavor = %q, want exact configured profile", flavor.Slug)
	}
}

func TestSelectInteractiveFlavorRejectsMissingConfiguredWireProfile(t *testing.T) {
	flavors := map[string]WireFlavor{
		"claude-code-interactive-default": {
			Slug: "claude-code-interactive-default",
			FeatureVectors: []WireFlavorFeatureVector{{
				ModelID: "claude-requested",
			}},
		},
	}

	_, err := selectInteractiveFlavor(flavors, WireFlavorFeatureVector{
		ModelID:     "claude-requested",
		WireProfile: "learned-missing",
	})
	if !errors.Is(err, ErrFlavorUnseeded) {
		t.Fatalf("error = %v, want ErrFlavorUnseeded", err)
	}
	if !strings.Contains(err.Error(), "learned-missing") {
		t.Fatalf("error = %q, want missing wire profile", err)
	}
}

func TestLoaderProjectsMissingFeatureVectorsAsZeroValue(t *testing.T) {
	store := seedStoreWithFlavors(t, []mitm.FlavorShape{testInteractiveFlavorShape()})

	flavors, err := newWireFlavorsLoader().Load(context.Background(), store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	flavor, ok := flavors["claude-code-interactive-17c1f069"]
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
	store := seedStoreWithFlavors(t, []mitm.FlavorShape{testAncillaryFlavorShape()})

	_, err := newWireFlavorsLoader().Load(context.Background(), store)
	if !errors.Is(err, ErrBaselineInvalid) {
		t.Fatalf("Load err = %v, want ErrBaselineInvalid", err)
	}
}

// TestLoaderMissingBaselineReturnsSentinel locks in that an empty store (no
// baseline row) yields ErrBaselineMissing so the adapter can map it to a 503.
func TestLoaderMissingBaselineReturnsSentinel(t *testing.T) {
	dbPath := t.TempDir() + "/capture.db"
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })

	if _, err := newWireFlavorsLoader().Load(context.Background(), store); !errors.Is(err, ErrBaselineMissing) {
		t.Fatalf("Load err = %v, want ErrBaselineMissing", err)
	}
}
