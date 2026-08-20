package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/mitm"
)

// wireBaselineConcern is the slog concern for wire-baseline load and
// projection diagnostics.
const wireBaselineConcern = "adapter.providers.anthropic.wire"

// anthropicWireUpstream is the upstream key the claude-code wire baseline is
// stored under in the capture store's baselines table.
const anthropicWireUpstream = "claude-code"

// ErrBaselineMissing reports that the MITM wire baseline does not exist
// in the capture store yet. The adapter maps this to an
// operator-actionable HTTP 503 rather than sending a wrong-shaped
// request with no identity headers.
var ErrBaselineMissing = errors.New("anthropic wire baseline missing")

// ErrBaselineInvalid reports that the stored MITM wire baseline exists
// but failed to parse or failed validation (no flavors, or no usable
// flavor carrying both a constant User-Agent and Anthropic-Version).
// Individual incomplete flavors are skipped with a warning rather than
// failing the load. The wrapped cause carries the specific failure.
var ErrBaselineInvalid = errors.New("anthropic wire baseline invalid")

// ErrFlavorUnseeded reports that the MITM wire baseline has no learned
// claude-cli flavor for the request's model and feature vector. The
// adapter maps this to the same HTTP 503 class as baseline load failures.
var ErrFlavorUnseeded = errors.New("anthropic wire flavor unseeded")

// anthropicBaselineStore is the slice of the capture store the anthropic
// wire-flavors loader needs: the current baseline JSON and a cheap updated-at
// token to poll for changes. The capture store satisfies it.
type anthropicBaselineStore interface {
	CurrentBaseline(ctx context.Context, upstream string) (json.RawMessage, bool, error)
	CurrentBaselineUpdatedAt(ctx context.Context, upstream string) (int64, error)
}

// WireFlavorsLoader reads the daemon-owned MITM baseline from the capture
// store and projects each captured flavor into a [WireFlavor]. There are no
// compiled-in defaults: the stored baseline is the single source of truth. The
// loader is concurrency-safe and caches the projected map keyed by the
// baseline's stored updated-at, re-reading only when it advances.
type WireFlavorsLoader struct {
	mu       sync.Mutex
	cached   map[string]WireFlavor
	cachedAt int64 // baseline updated_at (unix nanos) for the cached map
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

// Load reads the claude-code baseline from store, projects every captured
// flavor into a [WireFlavor], and returns them keyed by flavor slug. It polls
// the baseline's updated-at on every call and re-reads only when it advances;
// otherwise it returns the cached map. A missing baseline yields
// [ErrBaselineMissing]; a parse or validation failure yields
// [ErrBaselineInvalid] wrapping the cause.
func (l *WireFlavorsLoader) Load(ctx context.Context, store anthropicBaselineStore) (map[string]WireFlavor, error) {
	if store == nil {
		slog.WarnContext(ctx, "anthropic.wire_baseline.no_store", "concern", wireBaselineConcern, "subcomponent", "anthropic")
		return nil, fmt.Errorf("%w: no capture store", ErrBaselineMissing)
	}

	updatedAt, err := store.CurrentBaselineUpdatedAt(ctx, anthropicWireUpstream)
	if err != nil {
		slog.WarnContext(ctx, "anthropic.wire_baseline.updated_at_failed", "concern", wireBaselineConcern, "subcomponent", "anthropic",
			"err", err.Error(),
		)
		return nil, fmt.Errorf("%w: updated_at: %w", ErrBaselineInvalid, err)
	}
	if updatedAt == 0 {
		slog.WarnContext(ctx, "anthropic.wire_baseline.missing", "concern", wireBaselineConcern, "subcomponent", "anthropic")
		return nil, fmt.Errorf("%w: no baseline for %s", ErrBaselineMissing, anthropicWireUpstream)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cachedOK && l.cachedAt == updatedAt {
		return l.cached, nil
	}

	flavors, err := loadWireFlavorsFromStore(ctx, store)
	if err != nil {
		return nil, err
	}

	l.cached = flavors
	l.cachedAt = updatedAt
	l.cachedOK = true
	return flavors, nil
}

// loadWireFlavorsFromStore reads the claude-code baseline JSON from the store,
// decodes it, and projects each flavor. It reuses
// [mitm.DecodeBaselineSnapshot] because the mitm package does not import this
// package, so there is no import cycle.
func loadWireFlavorsFromStore(ctx context.Context, store anthropicBaselineStore) (map[string]WireFlavor, error) {
	raw, ok, err := store.CurrentBaseline(ctx, anthropicWireUpstream)
	if err != nil {
		slog.WarnContext(ctx, "anthropic.wire_baseline.read_failed", "concern", wireBaselineConcern, "subcomponent", "anthropic",
			"err", err.Error(),
		)
		return nil, fmt.Errorf("%w: read %s: %w", ErrBaselineInvalid, anthropicWireUpstream, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: no baseline for %s", ErrBaselineMissing, anthropicWireUpstream)
	}
	snap, err := mitm.DecodeBaselineSnapshot(raw)
	if err != nil {
		slog.WarnContext(ctx, "anthropic.wire_baseline.parse_failed", "concern", wireBaselineConcern, "subcomponent", "anthropic",
			"err", err.Error(),
		)
		return nil, fmt.Errorf("%w: parse %s: %w", ErrBaselineInvalid, anthropicWireUpstream, err)
	}
	if len(snap.Flavors) == 0 {
		slog.WarnContext(ctx, "anthropic.wire_baseline.no_flavors", "concern", wireBaselineConcern, "subcomponent", "anthropic")
		return nil, fmt.Errorf("%w: %s has no flavors", ErrBaselineInvalid, anthropicWireUpstream)
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
			slog.WarnContext(ctx, "anthropic.wire_baseline.flavor_missing_user_agent", "concern", wireBaselineConcern, "subcomponent", "anthropic",
				"slug", flavor.Slug,
			)
			continue
		}
		if strings.TrimSpace(flavor.AnthropicVersion) == "" {
			slog.WarnContext(ctx, "anthropic.wire_baseline.flavor_missing_version", "concern", wireBaselineConcern, "subcomponent", "anthropic",
				"slug", flavor.Slug,
			)
			continue
		}
		out[flavor.Slug] = flavor
	}
	if len(out) == 0 {
		slog.WarnContext(ctx, "anthropic.wire_baseline.no_usable_flavors", "concern", wireBaselineConcern, "subcomponent", "anthropic",
			"flavors_total", len(snap.Flavors),
		)
		return nil, fmt.Errorf("%w: %s has no usable flavors (all missing User-Agent or Anthropic-Version)", ErrBaselineInvalid, anthropicWireUpstream)
	}
	return out, nil
}

// projectFlavorShape maps one captured [mitm.FlavorShape] into a typed
// [WireFlavor]. It reads the verbatim value of each "constant"-classified
// identity header and copies the body field set.
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
		out = append(out, WireFlavorFeatureVector{
			ModelID:     feature.ModelID,
			WireProfile: "",
		})
	}
	return out
}

// constantHeaderValueFromShape returns the single observed value of a
// "constant"-classified header by name (case-insensitive). It returns
// "" when the header is not constant, not present, or carries more than
// one observed value.
func constantHeaderValueFromShape(shape mitm.FlavorShape, name string) string {
	for _, h := range shape.Headers {
		if !strings.EqualFold(h.Name, name) {
			continue
		}
		if h.Classification == mitm.HeaderClassConstant && len(h.ObservedValues) == 1 {
			return h.ObservedValues[0]
		}
		return ""
	}
	return ""
}

// wireFlavorExcludedHeaders is the set of header names projected into
// named fields or carrying secrets / per-session state, which must not
// appear in StaticHeaders.
//
// x-cc-atis is a claude-cli attestation token Clyde cannot mint. Replaying a
// captured one is not neutral: on claude-fable-5 the upstream answers a
// request carrying a stale token with a thinking block that has an encrypted
// signature and no plaintext, so the reasoning text disappears. Measured over
// five paired requests differing only in this header, fable returned 48, 48,
// 141, 4, and 48 plaintext characters without it and 0 every time with it;
// claude-sonnet-5 and claude-opus-5 were unaffected either way. Current
// claude-cli sends no such header, so dropping it matches the live client.
var wireFlavorExcludedHeaders = map[string]bool{
	"user-agent":               true,
	"anthropic-beta":           true,
	"anthropic-version":        true,
	"authorization":            true,
	"x-claude-code-session-id": true,
	"x-cc-atis":                true,
	"content-length":           true,
	"content-type":             true,
	"accept":                   true,
	"accept-encoding":          true,
	"connection":               true,
	"host":                     true,
}

// constantStaticHeadersFromShape returns every "constant" header except
// the well-known ones surfaced as named fields and the secret /
// per-session ones. Sorted by canonical Name.
func constantStaticHeadersFromShape(shape mitm.FlavorShape) []WireHeader {
	out := make([]WireHeader, 0, len(shape.Headers))
	for _, h := range shape.Headers {
		if wireFlavorExcludedHeaders[strings.ToLower(h.Name)] {
			continue
		}
		if h.Classification != mitm.HeaderClassConstant {
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
// whose presence is required across the flavor's records.
func requiredBodyFieldsFromShape(shape mitm.FlavorShape) []string {
	out := make([]string, 0, len(shape.Body.Fields))
	for _, f := range shape.Body.Fields {
		if f.Presence == mitm.HeaderPresenceRequired {
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
// trimmed, non-empty parts.
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
// "x-stainless-arch" to "X-Stainless-Arch").
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
