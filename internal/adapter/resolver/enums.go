package resolver

import (
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

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

// Effort is a named provider-owned reasoning tier string.
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

// Valid reports whether the effort is empty or a trimmed nonempty string.
// Exact model profiles validate membership before the resolver sees it;
// wildcard routes intentionally preserve future provider tiers.
func (e Effort) Valid() bool {
	return string(e) == strings.TrimSpace(string(e))
}

// ParseEffort preserves a trimmed provider-owned effort string.
func ParseEffort(raw string) (Effort, bool) {
	if raw != strings.TrimSpace(raw) {
		return EffortUnset, false
	}
	return Effort(raw), true
}
