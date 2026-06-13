package anthropic

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/mitm"
)

// wireBaselineConcern is the slog concern for wire-baseline load and
// projection diagnostics.
const wireBaselineConcern = "adapter.providers.anthropic.wire"

// ErrBaselineMissing reports that the on-disk MITM wire baseline does
// not exist. The adapter maps this to an operator-actionable HTTP 503
// rather than sending a wrong-shaped request with no identity headers.
var ErrBaselineMissing = errors.New("anthropic wire baseline missing")

// ErrBaselineInvalid reports that the on-disk MITM wire baseline exists
// but failed to parse or failed validation (no flavors, or no usable
// flavor carrying both a constant User-Agent and Anthropic-Version).
// Individual incomplete flavors are skipped with a warning rather than
// failing the load. The wrapped cause carries the specific failure.
var ErrBaselineInvalid = errors.New("anthropic wire baseline invalid")

// ErrFlavorUnseeded reports that the MITM wire baseline has no learned
// claude-cli flavor for the request's model and feature vector. The
// adapter maps this to the same HTTP 503 class as baseline load failures.
var ErrFlavorUnseeded = errors.New("anthropic wire flavor unseeded")

// WireFlavorsLoader reads the daemon-owned MITM baseline and projects
// each captured flavor into a [WireFlavor]. There are no compiled-in
// defaults: the on-disk baseline is the single source of truth. The
// loader is concurrency-safe and caches the projected map keyed by the
// file's modification time, re-reading only when the file changes.
type WireFlavorsLoader struct {
	mu       sync.Mutex
	cached   map[string]WireFlavor
	cachedAt int64 // baseline file mtime (unix nanos) for the cached map
	cachedOK bool  // a successful load has populated the cache
}

// newWireFlavorsLoader builds an empty loader. The mutex zero value is
// ready to use; the cache fields populate on the first successful
// [WireFlavorsLoader.Load].
func newWireFlavorsLoader() *WireFlavorsLoader {
	return &WireFlavorsLoader{
		mu:       sync.Mutex{},
		cached:   nil,
		cachedAt: 0,
		cachedOK: false,
	}
}

// Load reads baselinePath, projects every captured flavor into a
// [WireFlavor], and returns them keyed by flavor slug. It stats the
// path on every call and re-reads only when the file's mtime changes;
// otherwise it returns the cached map. A missing file yields
// [ErrBaselineMissing]; a parse or validation failure yields
// [ErrBaselineInvalid] wrapping the cause.
func (l *WireFlavorsLoader) Load(baselinePath string) (map[string]WireFlavor, error) {
	path := strings.TrimSpace(baselinePath)
	if path == "" {
		slog.Warn("anthropic.wire_baseline.path_empty", "concern", wireBaselineConcern, "subcomponent", "anthropic")
		return nil, fmt.Errorf("%w: empty baseline path", ErrBaselineInvalid)
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("anthropic.wire_baseline.missing", "concern", wireBaselineConcern, "subcomponent", "anthropic",
				"path", path,
			)
			return nil, fmt.Errorf("%w: %s", ErrBaselineMissing, path)
		}
		slog.Warn("anthropic.wire_baseline.stat_failed", "concern", wireBaselineConcern, "subcomponent", "anthropic",
			"path", path,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("%w: stat %s: %w", ErrBaselineInvalid, path, err)
	}

	mtime := info.ModTime().UnixNano()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cachedOK && l.cachedAt == mtime {
		return l.cached, nil
	}

	flavors, err := loadWireFlavorsFromBaseline(path)
	if err != nil {
		return nil, err
	}

	l.cached = flavors
	l.cachedAt = mtime
	l.cachedOK = true
	return flavors, nil
}

// loadWireFlavorsFromBaseline parses the v2 TOML baseline and projects
// each flavor. It reuses [mitm.LoadSnapshotV2TOML] because the mitm
// package does not import this package, so there is no import cycle.
func loadWireFlavorsFromBaseline(path string) (map[string]WireFlavor, error) {
	snap, err := mitm.LoadSnapshotV2TOML(path)
	if err != nil {
		slog.Warn("anthropic.wire_baseline.parse_failed", "concern", wireBaselineConcern, "subcomponent", "anthropic",
			"path", path,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("%w: parse %s: %w", ErrBaselineInvalid, path, err)
	}
	if len(snap.Flavors) == 0 {
		slog.Warn("anthropic.wire_baseline.no_flavors", "concern", wireBaselineConcern, "subcomponent", "anthropic",
			"path", path,
		)
		return nil, fmt.Errorf("%w: %s has no flavors", ErrBaselineInvalid, path)
	}

	// Incomplete flavors are skipped, not fatal: the learn loop also
	// captures non-interactive caller flavors (bootstrap GETs, probes,
	// axios sidecar calls) whose identity headers are absent or
	// non-constant, and those must not invalidate the interactive
	// flavor the egress actually selects. Only an entirely unusable
	// baseline is an error.
	out := make(map[string]WireFlavor, len(snap.Flavors))
	for _, shape := range snap.Flavors {
		flavor := projectFlavorShape(shape)
		if strings.TrimSpace(flavor.UserAgent) == "" {
			slog.Warn("anthropic.wire_baseline.flavor_missing_user_agent", "concern", wireBaselineConcern, "subcomponent", "anthropic",
				"path", path,
				"slug", flavor.Slug,
			)
			continue
		}
		if strings.TrimSpace(flavor.AnthropicVersion) == "" {
			slog.Warn("anthropic.wire_baseline.flavor_missing_version", "concern", wireBaselineConcern, "subcomponent", "anthropic",
				"path", path,
				"slug", flavor.Slug,
			)
			continue
		}
		out[flavor.Slug] = flavor
	}
	if len(out) == 0 {
		slog.Warn("anthropic.wire_baseline.no_usable_flavors", "concern", wireBaselineConcern, "subcomponent", "anthropic",
			"path", path,
			"flavors_total", len(snap.Flavors),
		)
		return nil, fmt.Errorf("%w: %s has no usable flavors (all missing User-Agent or Anthropic-Version)", ErrBaselineInvalid, path)
	}
	return out, nil
}

// projectFlavorShape maps one captured [mitm.FlavorShape] into a typed
// [WireFlavor]. It mirrors the build-time projection in
// internal/mitm/codegen_v2.go exactly so the runtime output equals the
// values the committed generated file held for the same baseline.
func projectFlavorShape(shape mitm.FlavorShape) WireFlavor {
	userAgent := constantHeaderValueFromShape(shape, "user-agent")
	anthropicVersion := constantHeaderValueFromShape(shape, "anthropic-version")
	anthropicBeta := constantHeaderValueFromShape(shape, "anthropic-beta")

	return WireFlavor{
		Slug:               shape.Slug,
		UserAgent:          userAgent,
		AnthropicVersion:   anthropicVersion,
		AnthropicBeta:      anthropicBeta,
		BetaFlags:          splitBetaFlags(anthropicBeta),
		StaticHeaders:      constantStaticHeadersFromShape(shape),
		BodyFields:         copyStrings(shape.Signature.BodyKeys),
		BodyFieldsRequired: requiredBodyFieldsFromShape(shape),
		FeatureVectors:     featureVectorsFromShape(shape),
		BillingAttestation: strings.TrimSpace(shape.BillingAttestation),
	}
}

func featureVectorsFromShape(shape mitm.FlavorShape) []WireFlavorFeatureVector {
	if len(shape.FeatureVectors) == 0 {
		return nil
	}
	out := make([]WireFlavorFeatureVector, 0, len(shape.FeatureVectors))
	for _, feature := range shape.FeatureVectors {
		out = append(out, WireFlavorFeatureVector{ModelID: feature.ModelID})
	}
	return out
}

// constantHeaderValueFromShape returns the single observed value of a
// "constant"-classified header by name (case-insensitive). It returns
// "" when the header is not constant, not present, or carries more than
// one observed value. Mirrors codegen_v2.constantHeaderValue.
func constantHeaderValueFromShape(shape mitm.FlavorShape, name string) string {
	for _, h := range shape.Headers {
		if !strings.EqualFold(h.Name, name) {
			continue
		}
		if h.Classification == mitm.V2HeaderClassConstant && len(h.ObservedValues) == 1 {
			return h.ObservedValues[0]
		}
		return ""
	}
	return ""
}

// wireFlavorExcludedHeaders is the set of header names projected into
// named fields or carrying secrets / per-session state, which must not
// appear in StaticHeaders. It mirrors the excluded map in
// codegen_v2.constantStaticHeaders.
var wireFlavorExcludedHeaders = map[string]bool{
	"user-agent":               true,
	"anthropic-beta":           true,
	"anthropic-version":        true,
	"authorization":            true,
	"x-claude-code-session-id": true,
	"content-length":           true,
	"content-type":             true,
	"accept":                   true,
	"accept-encoding":          true,
	"connection":               true,
	"host":                     true,
}

// constantStaticHeadersFromShape returns every "constant" header except
// the well-known ones surfaced as named fields and the secret /
// per-session ones. Sorted by canonical Name. Mirrors
// codegen_v2.constantStaticHeaders.
func constantStaticHeadersFromShape(shape mitm.FlavorShape) []WireHeader {
	out := make([]WireHeader, 0, len(shape.Headers))
	for _, h := range shape.Headers {
		if wireFlavorExcludedHeaders[strings.ToLower(h.Name)] {
			continue
		}
		if h.Classification != mitm.V2HeaderClassConstant {
			continue
		}
		if len(h.ObservedValues) != 1 {
			continue
		}
		out = append(out, WireHeader{
			Name:           canonicalWireHeaderName(h.Name),
			Value:          h.ObservedValues[0],
			Classification: string(h.Classification),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 {
		return nil
	}
	return out
}

// requiredBodyFieldsFromShape returns the sorted names of body fields
// whose presence is required across the flavor's records. Mirrors
// codegen_v2.requiredBodyFields.
func requiredBodyFieldsFromShape(shape mitm.FlavorShape) []string {
	out := make([]string, 0, len(shape.Body.Fields))
	for _, f := range shape.Body.Fields {
		if f.Presence == mitm.V2HeaderPresenceRequired {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitBetaFlags splits a comma-joined anthropic-beta value into its
// trimmed, non-empty parts. Mirrors codegen_v2.splitBetaFlags.
func splitBetaFlags(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// canonicalWireHeaderName canonical-cases a header name (e.g.
// "x-stainless-arch" to "X-Stainless-Arch"). Mirrors
// codegen_v2.canonicalHeaderName.
func canonicalWireHeaderName(name string) string {
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, "-")
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
