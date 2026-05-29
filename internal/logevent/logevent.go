// Package logevent defines Clyde's typed request-logging contract.
package logevent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Surface names the product surface that owns a request story.
type Surface string

const (
	// SurfaceAdapterChat identifies the adapter chat ingress surface.
	SurfaceAdapterChat Surface = "adapter_chat"
	// SurfaceMITMIDE identifies the MITM IDE backend surface.
	SurfaceMITMIDE Surface = "mitm_ide_backend"
)

// RouteFamily names the HTTP or proxy route family for a request story.
type RouteFamily string

const (
	// RouteFamilyChatCompatible identifies the adapter's SDK-shaped
	// chat routes.
	RouteFamilyChatCompatible RouteFamily = "chat_compatible"
	// RouteFamilyProviderNative identifies provider-native adapter
	// routes that expose a vendor wire contract directly.
	RouteFamilyProviderNative RouteFamily = "provider_native"
	// RouteFamilyMITMProxy identifies MITM proxy traffic.
	RouteFamilyMITMProxy RouteFamily = "mitm_proxy"
)

// Leg names a required request-story step.
type Leg string

const (
	// LegAdapterIngress records adapter ingress receipt.
	LegAdapterIngress Leg = "adapter_ingress"
	// LegAdapterPayload records adapter inline payload handling.
	LegAdapterPayload Leg = "adapter_payload"
	// LegAdapterClientMetadata records adapter ingress client-identity
	// metadata extraction. The leg is provider-neutral; provider
	// packages attach typed facet contributions to fold their
	// specific identity headers onto the event without naming a
	// provider in the generic leg constant.
	LegAdapterClientMetadata Leg = "adapter_client_metadata"
	// LegAdapterModelResolve records adapter model resolution.
	LegAdapterModelResolve Leg = "adapter_model_resolve"
	// LegProviderSendStarted records provider request dispatch start.
	LegProviderSendStarted Leg = "provider_send_started"
	// LegProviderAccepted records provider request acceptance.
	LegProviderAccepted Leg = "provider_accepted"
	// LegProviderResponseStarted records provider response stream start.
	LegProviderResponseStarted Leg = "provider_response_started"
	// LegProviderResponseDone records provider response completion.
	LegProviderResponseDone Leg = "provider_response_done"
	// LegAdapterRender records adapter response rendering.
	LegAdapterRender Leg = "adapter_render"
	// LegAdapterClientEgress records adapter client egress completion.
	LegAdapterClientEgress Leg = "adapter_client_egress"
	// LegRequestError records an early request error.
	LegRequestError Leg = "request_error"

	// LegMITMIngress records MITM ingress receipt.
	LegMITMIngress Leg = "mitm_ingress"
	// LegMITMPayload records MITM payload handling.
	LegMITMPayload Leg = "mitm_payload"
	// LegMITMUpstreamSend records MITM upstream dispatch.
	LegMITMUpstreamSend Leg = "mitm_upstream_send"
	// LegMITMUpstreamStart records MITM upstream response start.
	LegMITMUpstreamStart Leg = "mitm_upstream_start"
	// LegMITMForward records MITM client forwarding.
	LegMITMForward Leg = "mitm_forward"
	// LegMITMCaptureIndex records MITM capture index writing.
	LegMITMCaptureIndex Leg = "mitm_capture_index"
	// LegMITMComplete records MITM request completion.
	LegMITMComplete Leg = "mitm_complete"
)

// Phase names the lifecycle phase for one request leg.
type Phase string

const (
	// PhaseStarted records the start of a request leg.
	PhaseStarted Phase = "started"
	// PhaseCompleted records successful completion of a request leg.
	PhaseCompleted Phase = "completed"
	// PhaseFailed records failed completion of a request leg.
	PhaseFailed Phase = "failed"
)

// Status names the normalized request outcome.
type Status string

const (
	// StatusOK records a successful request outcome.
	StatusOK Status = "ok"
	// StatusError records a failed request outcome.
	StatusError Status = "error"
)

// SinkName identifies a stable logging sink selected by the central emitter.
type SinkName string

const (
	// SinkProcess routes events to the process log.
	SinkProcess SinkName = "process"
	// SinkConcern routes events to concern logs.
	SinkConcern SinkName = "concern"
	// SinkPerRequest routes events to per-request logs.
	SinkPerRequest SinkName = "per_request"
	// SinkPerChat routes events to per-chat logs.
	SinkPerChat SinkName = "per_chat"
	// SinkMITMCapture routes events to the MITM capture index.
	SinkMITMCapture SinkName = "mitm_capture_index"
	// SinkMITMRaw identifies MITM raw sidecar captures.
	SinkMITMRaw SinkName = "mitm_raw"
	// SinkProviderSidecar routes events to provider sidecar logs.
	SinkProviderSidecar SinkName = "provider_sidecar"
	// SinkInventory routes events to the inventory index.
	SinkInventory SinkName = "inventory_index"
)

// Identity carries provider-neutral request identity fields shared
// across all events. Provider-owned identity attaches via typed
// [Facet] contributions registered by the owning package.
type Identity struct {
	TraceID            string
	SpanID             string
	ParentSpanID       string
	RequestID          string
	UpstreamRequestID  string
	UpstreamResponseID string
	ChatKey            string
	ChatKeySource      string
	ChatRootKey        string
	ChatBranchKey      string
	SessionID          string
}

// Path carries path and routing fields shared across all events.
type Path struct {
	Surface     Surface
	RouteFamily RouteFamily
	Path        string
	Method      string
	Host        string
	Backend     string
	Provider    string
	UpstreamURL string
	Leg         Leg
	Phase       Phase
}

// Outcome carries normalized outcome fields.
type Outcome struct {
	Status       Status
	StatusCode   int
	ErrorCode    string
	ErrorMessage string
	Duration     time.Duration
	BytesIn      int64
	BytesOut     int64
}

// Facet is the typed provider-neutral contribution a provider,
// transport, or surface package attaches to an [Event]. The generic
// emitter only calls these methods on a Facet; provider packages
// own the concrete Go type, the JSON wire shape, and the
// slog-attribute names under their facet key.
//
// Implementations should be value types or pointers to small structs.
// Two facets that share a [FacetKey] collide; the last one
// registered through [Facets.Set] wins.
type Facet interface {
	// FacetKey returns the stable, lower-case wire key under which
	// this facet appears in the emitted JSON object and slog group.
	// The generic layer never branches on the value of this key.
	FacetKey() string
	// FacetAttrs returns the slog attributes this facet emits on the
	// event. The generic emitter wraps the returned attrs in a
	// [slog.Group] keyed by [FacetKey].
	FacetAttrs() []slog.Attr
	// SinkHints returns the typed sink-routing hints this facet
	// contributes. The generic sink selector reads only the
	// aggregated [SinkHints]; it does not branch on FacetKey.
	SinkHints() SinkHints
}

// SinkHints carries the typed routing hints a [Facet] contributes to
// an event. The generic sink selector ORs the hints across all
// facets on the event; no field in this struct names a provider.
type SinkHints struct {
	// HasRawCapture indicates the event references raw request and
	// response sidecar paths that must mirror to [SinkMITMRaw].
	HasRawCapture bool
	// NeedsProviderSidecar indicates the event should mirror to
	// [SinkProviderSidecar]. Provider packages set this hint on
	// facets whose payloads carry provider-specific telemetry the
	// dedicated sidecar log must preserve.
	NeedsProviderSidecar bool
}

// Merge combines two SinkHints. Either field set to true on either
// argument results in true on the returned value.
func (s SinkHints) Merge(other SinkHints) SinkHints {
	return SinkHints{
		HasRawCapture:        s.HasRawCapture || other.HasRawCapture,
		NeedsProviderSidecar: s.NeedsProviderSidecar || other.NeedsProviderSidecar,
	}
}

// Facets is the typed bundle of [Facet] contributions on an [Event].
// The bundle preserves registration order so JSON output is stable
// for a given event.
type Facets struct {
	items []Facet
}

// Set installs facet, replacing any prior facet whose [FacetKey]
// matches. A nil facet or a facet with an empty FacetKey is ignored.
func (f *Facets) Set(facet Facet) {
	if facet == nil {
		return
	}
	key := facet.FacetKey()
	if key == "" {
		return
	}
	for i, existing := range f.items {
		if existing.FacetKey() == key {
			f.items[i] = facet
			return
		}
	}
	f.items = append(f.items, facet)
}

// Get returns the registered facet for the supplied key.
func (f *Facets) Get(key string) (Facet, bool) {
	for _, existing := range f.items {
		if existing.FacetKey() == key {
			return existing, true
		}
	}
	return nil, false
}

// Len returns the number of facets currently registered on the
// bundle.
func (f *Facets) Len() int { return len(f.items) }

// Items returns the registered facets in insertion order. The
// returned slice is a shared view; callers must not mutate it.
func (f *Facets) Items() []Facet {
	if len(f.items) == 0 {
		return nil
	}
	out := make([]Facet, len(f.items))
	copy(out, f.items)
	return out
}

// MarshalJSON encodes the facet bundle as a JSON object keyed by
// each facet's [FacetKey], with values rendered by the facet's own
// JSON form. Provider packages own the wire shape under their key;
// the generic encoder does not interpret the values.
func (f *Facets) MarshalJSON() ([]byte, error) {
	if len(f.items) == 0 {
		return []byte("null"), nil
	}
	var buf []byte
	buf = append(buf, '{')
	for index, facet := range f.items {
		if index > 0 {
			buf = append(buf, ',')
		}
		keyBytes, err := json.Marshal(facet.FacetKey())
		if err != nil {
			return nil, fmt.Errorf("logevent: marshal facet key %q: %w", facet.FacetKey(), err)
		}
		buf = append(buf, keyBytes...)
		buf = append(buf, ':')
		valueBytes, err := json.Marshal(facet)
		if err != nil {
			return nil, fmt.Errorf("logevent: marshal facet value for %q: %w", facet.FacetKey(), err)
		}
		buf = append(buf, valueBytes...)
	}
	buf = append(buf, '}')
	return buf, nil
}

// AggregateSinkHints returns the union of every registered facet's
// [SinkHints]. The generic sink selector uses this aggregate without
// naming any facet.
func (f *Facets) AggregateSinkHints() SinkHints {
	var hints SinkHints
	for _, facet := range f.items {
		hints = hints.Merge(facet.SinkHints())
	}
	return hints
}

// Event is the central typed event shape for request traffic.
type Event struct {
	Time     time.Time
	Name     string
	Identity Identity
	Path     Path
	Outcome  Outcome
	Payload  *PayloadView
	Facets   Facets
	Sinks    []SinkName
}

// Attrs converts the typed event to slog attributes in the canonical field order.
func (e Event) Attrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 48)
	attrs = appendIdentityAttrs(attrs, e.Identity)
	attrs = appendPathAttrs(attrs, e.Path)
	attrs = appendOutcomeAttrs(attrs, e.Outcome)
	if e.Payload != nil {
		attrs = appendPayloadAttrs(attrs, *e.Payload)
	}
	attrs = appendFacetAttrs(attrs, e.Facets)
	if len(e.Sinks) > 0 {
		attrs = append(attrs, slog.Any("sinks", e.Sinks))
	}
	return attrs
}

func appendIdentityAttrs(attrs []slog.Attr, identity Identity) []slog.Attr {
	if identity.TraceID != "" {
		attrs = append(attrs, slog.String("trace_id", identity.TraceID))
	}
	if identity.SpanID != "" {
		attrs = append(attrs, slog.String("span_id", identity.SpanID))
	}
	if identity.ParentSpanID != "" {
		attrs = append(attrs, slog.String("parent_span_id", identity.ParentSpanID))
	}
	if identity.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", identity.RequestID))
	}
	if identity.UpstreamRequestID != "" {
		attrs = append(attrs, slog.String("upstream_request_id", identity.UpstreamRequestID))
	}
	if identity.UpstreamResponseID != "" {
		attrs = append(attrs, slog.String("upstream_response_id", identity.UpstreamResponseID))
	}
	if identity.ChatKey != "" {
		attrs = append(attrs, slog.String("chat_key", identity.ChatKey))
	}
	if identity.ChatKeySource != "" {
		attrs = append(attrs, slog.String("chat_key_source", identity.ChatKeySource))
	}
	if identity.ChatRootKey != "" {
		attrs = append(attrs, slog.String("chat_root_key", identity.ChatRootKey))
	}
	if identity.ChatBranchKey != "" {
		attrs = append(attrs, slog.String("chat_branch_key", identity.ChatBranchKey))
	}
	if identity.SessionID != "" {
		attrs = append(attrs, slog.String("session_id", identity.SessionID))
	}
	return attrs
}

func appendPathAttrs(attrs []slog.Attr, path Path) []slog.Attr {
	if component := componentForSurface(path.Surface); component != "" {
		attrs = append(attrs, slog.String("component", component))
	}
	attrs = append(attrs,
		slog.String("surface", string(path.Surface)),
		slog.String("route_family", string(path.RouteFamily)),
		slog.String("leg", string(path.Leg)),
		slog.String("phase", string(path.Phase)),
	)
	if path.Path != "" {
		attrs = append(attrs, slog.String("path", path.Path))
	}
	if path.Method != "" {
		attrs = append(attrs, slog.String("method", path.Method))
	}
	if path.Host != "" {
		attrs = append(attrs, slog.String("host", path.Host))
	}
	if path.Backend != "" {
		attrs = append(attrs, slog.String("backend", path.Backend))
	}
	if path.Provider != "" {
		attrs = append(attrs, slog.String("provider", path.Provider))
	}
	if path.UpstreamURL != "" {
		attrs = append(attrs, slog.String("upstream_url", path.UpstreamURL))
	}
	return attrs
}

func componentForSurface(surface Surface) string {
	switch surface {
	case SurfaceAdapterChat:
		return "adapter"
	case SurfaceMITMIDE:
		return "mitm"
	default:
		return ""
	}
}

func appendOutcomeAttrs(attrs []slog.Attr, outcome Outcome) []slog.Attr {
	if outcome.Status != "" {
		attrs = append(attrs, slog.String("status", string(outcome.Status)))
	}
	if outcome.StatusCode != 0 {
		attrs = append(attrs, slog.Int("status_code", outcome.StatusCode))
	}
	if outcome.ErrorCode != "" {
		attrs = append(attrs, slog.String("error_code", outcome.ErrorCode))
	}
	if outcome.ErrorMessage != "" {
		attrs = append(attrs, slog.String("error_message", outcome.ErrorMessage))
	}
	if outcome.Duration > 0 {
		attrs = append(attrs, slog.Int64("duration_ms", outcome.Duration.Milliseconds()))
	}
	if outcome.BytesIn > 0 {
		attrs = append(attrs, slog.Int64("bytes_in", outcome.BytesIn))
	}
	if outcome.BytesOut > 0 {
		attrs = append(attrs, slog.Int64("bytes_out", outcome.BytesOut))
	}
	return attrs
}

func appendFacetAttrs(attrs []slog.Attr, facets Facets) []slog.Attr {
	for _, facet := range facets.items {
		facetAttrs := facet.FacetAttrs()
		if len(facetAttrs) == 0 {
			continue
		}
		attrs = append(attrs, slog.Attr{Key: facet.FacetKey(), Value: slog.GroupValue(facetAttrs...)})
	}
	return attrs
}

// RequiredLegs defines required legs by request surface.
type RequiredLegs map[Surface][]Leg

// IncompletePolicy controls how an incomplete request story is enforced.
type IncompletePolicy string

const (
	// IncompletePolicyWarn emits logging.request.incomplete without invoking a
	// wired [IncompleteHandler]. This is the production default.
	IncompletePolicyWarn IncompletePolicy = "warn"
	// IncompletePolicyFailTest emits logging.request.incomplete and invokes any
	// wired [IncompleteHandler] (typically wrapping [testing.TB].Fatalf in tests).
	IncompletePolicyFailTest IncompletePolicy = "fail_test"
)

// ParseIncompletePolicy returns the typed policy for a config value.
func ParseIncompletePolicy(value string) (IncompletePolicy, bool) {
	policy := IncompletePolicy(strings.ToLower(strings.TrimSpace(value)))
	switch policy {
	case IncompletePolicyWarn, IncompletePolicyFailTest:
		return policy, true
	default:
		return "", false
	}
}

// DefaultRequiredLegs returns the canonical required request path legs.
func DefaultRequiredLegs() RequiredLegs {
	return RequiredLegs{
		SurfaceAdapterChat: {
			LegAdapterIngress,
			LegAdapterPayload,
			LegAdapterClientMetadata,
			LegAdapterModelResolve,
			LegProviderSendStarted,
			LegProviderAccepted,
			LegProviderResponseStarted,
			LegProviderResponseDone,
			LegAdapterRender,
			LegAdapterClientEgress,
		},
		SurfaceMITMIDE: {
			LegMITMIngress,
			LegMITMPayload,
			LegMITMUpstreamSend,
			LegMITMUpstreamStart,
			LegMITMForward,
			LegMITMCaptureIndex,
			LegMITMComplete,
		},
	}
}

// RequiredLegsFromStrings converts config wire values into the typed contract.
func RequiredLegsFromStrings(values map[string][]string) RequiredLegs {
	if len(values) == 0 {
		return DefaultRequiredLegs()
	}
	parsed := make(RequiredLegs, len(values))
	for surfaceName, legNames := range values {
		surface, ok := ParseSurface(surfaceName)
		if !ok {
			continue
		}
		legs := make([]Leg, 0, len(legNames))
		for _, legName := range legNames {
			leg, legOK := ParseLeg(legName)
			if legOK {
				legs = append(legs, leg)
			}
		}
		if len(legs) > 0 {
			parsed[surface] = legs
		}
	}
	if len(parsed) == 0 {
		return DefaultRequiredLegs()
	}
	return parsed
}

// ParseSurface returns a typed request surface for a config value.
func ParseSurface(value string) (Surface, bool) {
	surface := Surface(strings.ToLower(strings.TrimSpace(value)))
	switch surface {
	case SurfaceAdapterChat, SurfaceMITMIDE:
		return surface, true
	default:
		return "", false
	}
}

// ParseLeg returns a typed request leg for a config value.
func ParseLeg(value string) (Leg, bool) {
	leg := Leg(strings.ToLower(strings.TrimSpace(value)))
	switch leg {
	case LegAdapterIngress,
		LegAdapterPayload,
		LegAdapterClientMetadata,
		LegAdapterModelResolve,
		LegProviderSendStarted,
		LegProviderAccepted,
		LegProviderResponseStarted,
		LegProviderResponseDone,
		LegAdapterRender,
		LegAdapterClientEgress,
		LegRequestError,
		LegMITMIngress,
		LegMITMPayload,
		LegMITMUpstreamSend,
		LegMITMUpstreamStart,
		LegMITMForward,
		LegMITMCaptureIndex,
		LegMITMComplete:
		return leg, true
	default:
		return "", false
	}
}

// IncompleteReport carries typed details about a missing-leg condition that
// reaches [Recorder.Complete] with [IncompletePolicyFailTest] selected.
type IncompleteReport struct {
	Surface     Surface
	RouteFamily RouteFamily
	Missing     []Leg
	Observed    []Leg
	LastPhase   Phase
	DurationMS  int64
	Identity    Identity
}

// IncompleteHandler is invoked from [Recorder.Complete] when missing legs are
// observed and the emitter's policy is [IncompletePolicyFailTest]. Production
// callers must wire a nil handler; test helpers wire a non-nil handler that
// fails the active test (typically by invoking [testing.TB.Fatalf] on the
// wrapped reporter).
type IncompleteHandler func(report IncompleteReport)

// Emitter owns request event emission and required-leg enforcement.
type Emitter struct {
	logger       *slog.Logger
	required     RequiredLegs
	now          func() time.Time
	policy       IncompletePolicy
	onIncomplete IncompleteHandler
}

// EmitterOption configures an Emitter at construction time.
type EmitterOption func(*Emitter)

// WithIncompletePolicy sets the incomplete-request enforcement policy.
//
// The default policy when no option is supplied is [IncompletePolicyWarn]. An
// empty policy value is rejected and reverts to [IncompletePolicyWarn] so the
// emitter never carries an unparseable policy.
func WithIncompletePolicy(policy IncompletePolicy) EmitterOption {
	return func(e *Emitter) {
		switch policy {
		case IncompletePolicyWarn, IncompletePolicyFailTest:
			e.policy = policy
		default:
			e.policy = IncompletePolicyWarn
		}
	}
}

// WithTestingTB wires a fail handler the emitter invokes when [Recorder.Complete]
// observes missing legs under [IncompletePolicyFailTest].
//
// The unified logging refactor plan asked for a "WithTestingTB" option that
// test helpers use. To respect the project type-safety rule against importing
// [testing.TB] (whose [testing.TB.Fatalf] is variadic any) into production
// code, this option accepts a typed [IncompleteHandler] callback instead. Test
// callers adapt [testing.TB] inside the callback they pass; production callers
// wire a nil handler.
func WithTestingTB(handler IncompleteHandler) EmitterOption {
	return func(e *Emitter) {
		e.onIncomplete = handler
	}
}

// NewEmitter returns a central request event emitter.
func NewEmitter(logger *slog.Logger, required RequiredLegs, opts ...EmitterOption) *Emitter {
	if logger == nil {
		logger = slog.Default()
	}
	if len(required) == 0 {
		required = DefaultRequiredLegs()
	}
	emitter := &Emitter{
		logger:       logger,
		required:     cloneRequiredLegs(required),
		now:          time.Now,
		policy:       IncompletePolicyWarn,
		onIncomplete: nil,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(emitter)
	}
	return emitter
}

// Emit writes a standalone typed event without starting required-leg tracking.
func (e *Emitter) Emit(ctx context.Context, event Event) {
	if e == nil {
		e = NewEmitter(nil, nil)
	}
	if len(event.Sinks) == 0 {
		event.Sinks = DefaultSinksForEvent(event)
	}
	if event.Time.IsZero() {
		event.Time = e.now()
	}
	if event.Name == "" {
		event.Name = "logging.request.leg"
	}
	e.logger.LogAttrs(ctx, slog.LevelInfo, event.Name, append([]slog.Attr{slog.String("concern", "adapter.chat.dispatch")}, event.Attrs()...)...)
}

// Begin starts a request story.
func (e *Emitter) Begin(identity Identity, path Path) *Recorder {
	if e == nil {
		e = NewEmitter(nil, nil)
	}
	return &Recorder{
		mu:        sync.Mutex{},
		emitter:   e,
		identity:  identity,
		basePath:  path,
		observed:  make(map[Leg]bool),
		lastPhase: "",
		started:   e.now(),
		completed: false,
	}
}

// Recorder records one request story and emits incomplete warnings at completion.
type Recorder struct {
	mu        sync.Mutex
	emitter   *Emitter
	identity  Identity
	basePath  Path
	observed  map[Leg]bool
	lastPhase Phase
	started   time.Time
	completed bool
}

// UpdateIdentity updates the story identity after request metadata is discovered.
func (r *Recorder) UpdateIdentity(identity Identity) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identity = identity
}

// Emit writes one typed request event and records its leg.
func (r *Recorder) Emit(ctx context.Context, event Event) {
	if r == nil || r.emitter == nil {
		return
	}
	r.mu.Lock()
	if event.Identity.RequestID == "" {
		event.Identity = r.identity
	}
	if event.Path.Surface == "" {
		event.Path.Surface = r.basePath.Surface
	}
	if event.Path.RouteFamily == "" {
		event.Path.RouteFamily = r.basePath.RouteFamily
	}
	if event.Path.Path == "" {
		event.Path.Path = r.basePath.Path
	}
	if event.Path.Method == "" {
		event.Path.Method = r.basePath.Method
	}
	if event.Path.Host == "" {
		event.Path.Host = r.basePath.Host
	}
	if len(event.Sinks) == 0 {
		event.Sinks = DefaultSinksForEvent(event)
	}
	if event.Time.IsZero() {
		event.Time = r.emitter.now()
	}
	if event.Name == "" {
		event.Name = "logging.request.leg"
	}
	if event.Path.Leg != "" {
		r.observed[event.Path.Leg] = true
	}
	r.lastPhase = event.Path.Phase
	logger := r.emitter.logger
	r.mu.Unlock()
	logger.LogAttrs(ctx, slog.LevelInfo, event.Name, append([]slog.Attr{slog.String("concern", "adapter.chat.dispatch")}, event.Attrs()...)...)
}

// EmitError closes an early failure with the request error leg.
func (r *Recorder) EmitError(ctx context.Context, errorCode string, errorMessage string) {
	if r == nil {
		return
	}
	var event Event
	event.Name = "logging.request.leg"
	event.Path.Leg = LegRequestError
	event.Path.Phase = PhaseFailed
	event.Outcome.Status = StatusError
	event.Outcome.ErrorCode = errorCode
	event.Outcome.ErrorMessage = errorMessage
	event.Outcome.Duration = r.Duration()
	r.Emit(ctx, event)
}

// Duration returns elapsed request time.
func (r *Recorder) Duration() time.Duration {
	if r == nil || r.emitter == nil || r.started.IsZero() {
		return 0
	}
	return r.emitter.now().Sub(r.started)
}

// Complete emits logging.request.incomplete when a required leg is missing and,
// when the emitter's policy is [IncompletePolicyFailTest] and a [TestReporter]
// is wired, calls [TestReporter.Fatalf] on the same condition.
//
// Returns true when the request story is complete (no missing legs or observed
// [LegRequestError]) and false when an incomplete event was emitted.
func (r *Recorder) Complete(ctx context.Context) bool {
	if r == nil || r.emitter == nil {
		return true
	}
	r.mu.Lock()
	if r.completed {
		r.mu.Unlock()
		return true
	}
	r.completed = true
	identity := r.identity
	basePath := r.basePath
	observed := make(map[Leg]bool, len(r.observed))
	maps.Copy(observed, r.observed)
	lastPhase := r.lastPhase
	duration := r.emitter.now().Sub(r.started)
	required := append([]Leg(nil), r.emitter.required[basePath.Surface]...)
	logger := r.emitter.logger
	policy := r.emitter.policy
	onIncomplete := r.emitter.onIncomplete
	r.mu.Unlock()

	missing := missingLegs(required, observed)
	if len(missing) == 0 || observed[LegRequestError] {
		return true
	}
	observedSlice := observedLegs(observed)
	durationMS := duration.Milliseconds()
	attrs := []slog.Attr{
		slog.String("surface", string(basePath.Surface)),
		slog.String("route_family", string(basePath.RouteFamily)),
		slog.Any("expected_legs", required),
		slog.Any("observed_legs", observedSlice),
		slog.Any("missing_legs", missing),
		slog.String("last_phase", string(lastPhase)),
		slog.Int64("duration_ms", durationMS),
		slog.String("incomplete_policy", string(policy)),
	}
	attrs = appendIdentityAttrs(attrs, identity)
	logger.LogAttrs(ctx, slog.LevelWarn, "logging.request.incomplete", append([]slog.Attr{slog.String("concern", "adapter.chat.dispatch")}, attrs...)...)
	if policy == IncompletePolicyFailTest && onIncomplete != nil {
		onIncomplete(IncompleteReport{
			Surface:     basePath.Surface,
			RouteFamily: basePath.RouteFamily,
			Missing:     missing,
			Observed:    observedSlice,
			LastPhase:   lastPhase,
			DurationMS:  durationMS,
			Identity:    identity,
		})
	}
	return false
}

// DefaultSinksForEvent selects sinks for a request event in one
// place. The selector reads only typed generic fields plus the
// aggregated [SinkHints] contributed by registered facets, so the
// generic emit path never names a provider when choosing sinks.
func DefaultSinksForEvent(event Event) []SinkName {
	sinks := []SinkName{SinkProcess, SinkConcern, SinkInventory}
	if event.Identity.ChatKey != "" {
		sinks = append(sinks, SinkPerChat)
	}
	if event.Path.Surface == SurfaceMITMIDE {
		sinks = append(sinks, SinkMITMCapture)
	}
	hints := event.Facets.AggregateSinkHints()
	if hints.HasRawCapture {
		sinks = append(sinks, SinkMITMRaw)
	}
	if hints.NeedsProviderSidecar {
		sinks = append(sinks, SinkProviderSidecar)
	}
	return sinks
}

func missingLegs(required []Leg, observed map[Leg]bool) []Leg {
	missing := make([]Leg, 0)
	for _, leg := range required {
		if !observed[leg] {
			missing = append(missing, leg)
		}
	}
	return missing
}

func observedLegs(observed map[Leg]bool) []Leg {
	legs := make([]Leg, 0, len(observed))
	for leg := range observed {
		legs = append(legs, leg)
	}
	slices.Sort(legs)
	return legs
}

func cloneRequiredLegs(required RequiredLegs) RequiredLegs {
	cloned := make(RequiredLegs, len(required))
	for surface, legs := range required {
		cloned[surface] = append([]Leg(nil), legs...)
	}
	return cloned
}
