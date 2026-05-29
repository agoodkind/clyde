package resolver

import adaptermodel "goodkind.io/clyde/internal/adapter/model"

// ProviderID is the typed enum naming the upstream provider that the
// resolved request will be dispatched to. It is a type alias of the
// adapter's single provider-routing enum, so the resolver and the model
// registry share one set of provider-routing values. String() and
// Valid() come from the underlying adaptermodel.BackendID.
type ProviderID = adaptermodel.BackendID

const (
	// ProviderUnknown is the zero value. It signals that resolution
	// produced no provider mapping, typically because the upstream
	// model name was not registered. Callers must treat it as an
	// error condition; dispatchers must not try to look it up.
	ProviderUnknown = adaptermodel.BackendID("")
	// ProviderAnthropic dispatches to the Anthropic OAuth bucket
	// implementation in internal/adapter/anthropic.
	ProviderAnthropic = adaptermodel.BackendAnthropic
	// ProviderCodex dispatches to the Codex websocket implementation
	// in internal/adapter/codex.
	ProviderCodex = adaptermodel.BackendCodex
	// ProviderPassthrough dispatches to the passthrough-override path.
	ProviderPassthrough = adaptermodel.BackendPassthroughOverride
)

// Effort is the typed reasoning-effort enum carried on a ResolvedRequest.
// The set mirrors the values the existing model registry accepts; the
// resolver narrows the wire string to one of these canonical values
// (or returns an error at the boundary).
type Effort string

const (
	// EffortUnset is the zero value. The provider treats it as
	// "use family default".
	EffortUnset Effort = ""
	// EffortNone disables reasoning entirely.
	EffortNone Effort = "none"
	// EffortLow is the lowest declared reasoning tier.
	EffortLow Effort = "low"
	// EffortMedium is the middle declared reasoning tier.
	EffortMedium Effort = "medium"
	// EffortHigh is the highest commonly declared reasoning tier.
	EffortHigh Effort = "high"
	// EffortXHigh is the extended-thinking tier some families allow.
	EffortXHigh Effort = "xhigh"
	// EffortMax is the cap tier some families allow.
	EffortMax Effort = "max"
)

// String returns the wire-form value of the Effort.
func (e Effort) String() string {
	return string(e)
}

// Valid reports whether the Effort is one of the known typed values
// (including EffortUnset, since unset is a legitimate state).
func (e Effort) Valid() bool {
	switch e {
	case EffortUnset, EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return true
	}
	return false
}

// ParseEffort maps a wire-form effort string to a typed Effort. Empty
// input is EffortUnset. Whitespace is trimmed. An unrecognised value
// returns EffortUnset and false; callers may decide whether that is
// fatal at their boundary.
func ParseEffort(raw string) (Effort, bool) {
	switch raw {
	case "":
		return EffortUnset, true
	case string(EffortNone):
		return EffortNone, true
	case string(EffortLow):
		return EffortLow, true
	case string(EffortMedium):
		return EffortMedium, true
	case string(EffortHigh):
		return EffortHigh, true
	case string(EffortXHigh):
		return EffortXHigh, true
	case string(EffortMax):
		return EffortMax, true
	}
	return EffortUnset, false
}
