package codex

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

// codexWireBaselineConcern is the slog concern for codex wire-baseline
// load and projection diagnostics.
const codexWireBaselineConcern = "adapter.providers.codex.wire"

// codexWireUpstream is the upstream key the codex-cli wire baseline is stored
// under in the capture store's baselines table. It matches the drift writer's
// driftUpstreamCodexCLI value.
const codexWireUpstream = "codex-cli"

// codexWireFlavorSlugPrefix is the slug prefix the codex-cli capture
// flavor carries in the daemon-owned MITM baseline. The capture
// partitions one upstream into caller flavors; codex-cli's outbound
// Responses websocket traffic lands under this prefix.
const codexWireFlavorSlugPrefix = "codex-cli"

// Codex constant-header names as they appear (lowercased) in the
// captured drift records. The MITM drift writer captures these verbatim
// (research note: internal/mitm/drift_capture.go driftHeaderMap keeps
// identity and attestation headers unmasked), so the baseline records
// them as single-valued "constant" headers the loader projects below.
const (
	codexWireHeaderOriginator   = "originator"
	codexWireHeaderOpenAIBeta   = "openai-beta"
	codexWireHeaderUserAgent    = "user-agent"
	codexWireHeaderBetaFeatures = "x-codex-beta-features"
	codexWireHeaderAttestation  = "x-oai-attestation"
)

// ErrCodexBaselineMissing reports that the MITM wire baseline for
// codex-cli does not exist in the capture store yet. Unlike the
// Anthropic path, this is NOT a hard failure: the codex egress treats
// it as "no baseline" and falls back to the compiled-in identity
// constants so a cold-start codex still works.
var ErrCodexBaselineMissing = errors.New("codex wire baseline missing")

// ErrCodexBaselineInvalid reports that the stored MITM wire baseline
// exists but failed to parse, carries no flavors, or carries no
// codex-cli flavor. The codex egress treats this the same as missing:
// it falls back to the compiled-in constants rather than failing the
// request.
var ErrCodexBaselineInvalid = errors.New("codex wire baseline invalid")

// codexBaselineStore is the slice of the capture store the codex wire-baseline
// loader needs: the current baseline JSON and a cheap updated-at token to poll
// for changes. The capture store satisfies it.
type codexBaselineStore interface {
	CurrentBaseline(ctx context.Context, upstream string) (json.RawMessage, bool, error)
	CurrentBaselineUpdatedAt(ctx context.Context, upstream string) (int64, error)
}

// WireIdentity is the typed projection of one captured codex-cli wire
// flavor. Every field is the verbatim value observed on the wire for a
// "constant"-classified header, or the captured body field set. An
// empty field means the baseline did not capture that value, in which
// case the egress keeps its compiled-in constant for that field.
type WireIdentity struct {
	// Slug is the captured flavor slug the identity was projected from.
	Slug string
	// Originator is the captured `originator` header value (codex-rs
	// DEFAULT_ORIGINATOR, e.g. "codex_cli_rs").
	Originator string
	// OpenAIBeta is the captured `openai-beta` header value (the
	// responses-websocket upgrade version).
	OpenAIBeta string
	// UserAgent is the captured `user-agent` header value (the
	// codex-cli get_codex_user_agent string).
	UserAgent string
	// BetaFeatures is the captured `x-codex-beta-features` header value.
	BetaFeatures string
	// Attestation is the captured `x-oai-attestation` header value. The
	// egress replays it as the `X-Oai-Attestation` header when present.
	Attestation string
	// BodyFields is the captured top-level body key order observed for
	// this flavor. It is informational on the codex path: the outbound
	// body field order is fixed by Go struct field order in
	// transport_request.go (Go encodes JSON in struct-field order), so
	// the loader surfaces the learned order without reordering the
	// struct.
	BodyFields []string
	// BodyFieldsRequired is the sorted set of body field names observed
	// on every record of this flavor.
	BodyFieldsRequired []string
}

// WireBaselineLoader reads the daemon-owned MITM baseline from the capture
// store and projects the codex-cli flavor into a [WireIdentity]. It is
// concurrency-safe and caches the projection keyed by the baseline's stored
// updated-at, re-reading only when the baseline advances. A missing or invalid
// baseline yields a typed sentinel error so the caller can fall back to the
// compiled-in constants.
type WireBaselineLoader struct {
	mu       sync.Mutex
	cached   WireIdentity
	cachedAt int64 // baseline updated_at (unix nanos) for the cached identity
	cachedOK bool  // a successful load has populated the cache
}

// NewWireBaselineLoader builds an empty loader. The mutex zero value is
// ready to use; the cache fields populate on the first successful
// [WireBaselineLoader.Load].
func NewWireBaselineLoader() *WireBaselineLoader {
	var loader WireBaselineLoader
	return &loader
}

// Load reads the codex-cli baseline from store, projects the codex-cli flavor
// into a [WireIdentity], and returns it. It polls the baseline's updated-at on
// every call and re-reads only when it advances; otherwise it returns the
// cached identity. A missing baseline yields [ErrCodexBaselineMissing]; a
// parse, no-flavor, or no-codex-flavor failure yields
// [ErrCodexBaselineInvalid]. Both sentinels signal the caller to fall back to
// the compiled-in identity constants.
func (l *WireBaselineLoader) Load(ctx context.Context, store codexBaselineStore) (WireIdentity, error) {
	var zero WireIdentity
	if store == nil {
		return zero, fmt.Errorf("%w: no capture store", ErrCodexBaselineMissing)
	}

	updatedAt, err := store.CurrentBaselineUpdatedAt(ctx, codexWireUpstream)
	if err != nil {
		slog.WarnContext(ctx, "codex.wire_baseline.updated_at_failed", "concern", codexWireBaselineConcern, "subcomponent", "codex",
			"err", err.Error(),
		)
		return zero, fmt.Errorf("%w: updated_at: %w", ErrCodexBaselineInvalid, err)
	}
	if updatedAt == 0 {
		slog.DebugContext(ctx, "codex.wire_baseline.missing", "concern", codexWireBaselineConcern, "subcomponent", "codex")
		return zero, fmt.Errorf("%w: no baseline for %s", ErrCodexBaselineMissing, codexWireUpstream)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cachedOK && l.cachedAt == updatedAt {
		return l.cached, nil
	}

	identity, err := loadWireIdentityFromStore(ctx, store)
	if err != nil {
		return zero, err
	}

	l.cached = identity
	l.cachedAt = updatedAt
	l.cachedOK = true
	return identity, nil
}

// loadWireIdentityFromStore reads the codex-cli baseline JSON from the store,
// decodes it, and projects the codex-cli flavor. It reuses
// [mitm.DecodeBaselineSnapshot] because the mitm package does not import this
// package, so there is no import cycle.
func loadWireIdentityFromStore(ctx context.Context, store codexBaselineStore) (WireIdentity, error) {
	var zero WireIdentity
	raw, ok, err := store.CurrentBaseline(ctx, codexWireUpstream)
	if err != nil {
		slog.WarnContext(ctx, "codex.wire_baseline.read_failed", "concern", codexWireBaselineConcern, "subcomponent", "codex",
			"err", err.Error(),
		)
		return zero, fmt.Errorf("%w: read %s: %w", ErrCodexBaselineInvalid, codexWireUpstream, err)
	}
	if !ok {
		return zero, fmt.Errorf("%w: no baseline for %s", ErrCodexBaselineMissing, codexWireUpstream)
	}
	snap, err := mitm.DecodeBaselineSnapshot(raw)
	if err != nil {
		slog.WarnContext(ctx, "codex.wire_baseline.parse_failed", "concern", codexWireBaselineConcern, "subcomponent", "codex",
			"err", err.Error(),
		)
		return zero, fmt.Errorf("%w: parse %s: %w", ErrCodexBaselineInvalid, codexWireUpstream, err)
	}
	if len(snap.Flavors) == 0 {
		slog.DebugContext(ctx, "codex.wire_baseline.no_flavors", "concern", codexWireBaselineConcern, "subcomponent", "codex")
		return zero, fmt.Errorf("%w: %s has no flavors", ErrCodexBaselineInvalid, codexWireUpstream)
	}
	shape, ok := selectCodexFlavor(snap.Flavors)
	if !ok {
		slog.DebugContext(ctx, "codex.wire_baseline.no_codex_flavor", "concern", codexWireBaselineConcern, "subcomponent", "codex")
		return zero, fmt.Errorf("%w: no codex-cli flavor in baseline %s", ErrCodexBaselineInvalid, codexWireUpstream)
	}
	return projectWireIdentity(shape), nil
}

// selectCodexFlavor picks the codex-cli flavor from the captured
// flavors. The baseline can hold several flavors (one per observed
// codex-cli version); the first whose slug carries the codex-cli prefix
// is selected. When no slug carries the prefix but exactly one flavor
// exists, that single flavor is used so a minimally-seeded baseline
// still projects.
func selectCodexFlavor(flavors []mitm.FlavorShape) (mitm.FlavorShape, bool) {
	var zero mitm.FlavorShape
	for _, shape := range flavors {
		if strings.HasPrefix(shape.Slug, codexWireFlavorSlugPrefix) {
			return shape, true
		}
	}
	if len(flavors) == 1 {
		return flavors[0], true
	}
	return zero, false
}

// projectWireIdentity maps one captured [mitm.FlavorShape] into a typed
// [WireIdentity]. It reads the verbatim value of each
// "constant"-classified identity header and copies the body field set.
func projectWireIdentity(shape mitm.FlavorShape) WireIdentity {
	return WireIdentity{
		Slug:               shape.Slug,
		Originator:         codexConstantHeaderValue(shape, codexWireHeaderOriginator),
		OpenAIBeta:         codexConstantHeaderValue(shape, codexWireHeaderOpenAIBeta),
		UserAgent:          codexConstantHeaderValue(shape, codexWireHeaderUserAgent),
		BetaFeatures:       codexConstantHeaderValue(shape, codexWireHeaderBetaFeatures),
		Attestation:        codexConstantHeaderValue(shape, codexWireHeaderAttestation),
		BodyFields:         copyCodexStrings(shape.Signature.BodyKeys),
		BodyFieldsRequired: codexRequiredBodyFields(shape),
	}
}

// codexConstantHeaderValue returns the single observed value of a
// "constant"-classified header by name (case-insensitive). It returns
// "" when the header is not constant, not present, or carries more than
// one observed value, matching the Anthropic loader's projection rule.
func codexConstantHeaderValue(shape mitm.FlavorShape, name string) string {
	for _, h := range shape.Headers {
		if !strings.EqualFold(h.Name, name) {
			continue
		}
		if h.Classification == mitm.HeaderClassConstant && len(h.ObservedValues) == 1 {
			return strings.TrimSpace(h.ObservedValues[0])
		}
		return ""
	}
	return ""
}

// codexRequiredBodyFields returns the sorted names of body fields whose
// presence is required across the flavor's records.
func codexRequiredBodyFields(shape mitm.FlavorShape) []string {
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

func copyCodexStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
