package resolver

import (
	"fmt"
	"log/slog"
	"maps"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

// ModelRegistryAdapter wraps the existing model.Registry so it satisfies
// the resolver's ModelRegistry interface. It is the production binding
// between the resolver and the existing per-alias resolution logic.
//
// The adapter performs no IO. It calls model.Registry.Resolve and
// projects the returned model.ResolvedAlias into the resolver's typed
// ResolvedModelView. Only the fields the resolver needs are copied;
// everything else stays on the underlying ResolvedAlias and is
// available via downstream code paths that still consume the existing
// type.
type ModelRegistryAdapter struct {
	inner *adaptermodel.Registry
}

// NewModelRegistryAdapter binds an existing model.Registry to the
// resolver interface. A nil inner is allowed at construction so call
// sites can wire the adapter early; Resolve returns a typed error if
// invoked while the inner registry is nil.
func NewModelRegistryAdapter(inner *adaptermodel.Registry) *ModelRegistryAdapter {
	return &ModelRegistryAdapter{inner: inner}
}

// Resolve satisfies the ModelRegistry interface. It calls
// model.Registry.Resolve and projects the result.
func (a *ModelRegistryAdapter) Resolve(surface IngressSurface, alias, reqEffort string) (ResolvedModelView, error) {
	if a == nil || a.inner == nil {
		return ResolvedModelView{}, ErrUnresolvedProvider
	}
	resolved, effort, err := a.inner.Resolve(surface, alias, reqEffort)
	if err != nil {
		slog.Warn("adapter.resolver.bridge_resolve_failed", "concern", "adapter.models.resolve", "alias", alias, "err", err)
		if kind, ok := adaptermodel.ResolveErrorKindOf(err); ok && kind == adaptermodel.ResolveErrorInvalidRequest {
			return ResolvedModelView{}, &InvalidRequestError{message: err.Error(), cause: err}
		}
		return ResolvedModelView{}, fmt.Errorf("resolve model alias %s: %w", alias, err)
	}
	provider := resolved.Backend
	parsedEffort, _ := ParseEffort(effort)
	parsedWireEffort, _ := ParseEffort(resolved.WireEffort)
	if parsedWireEffort == EffortUnset {
		parsedWireEffort = parsedEffort
	}
	family := resolved.Profile
	if family == "" {
		family = resolved.WireModel
	}
	return ResolvedModelView{
		Provider:                provider,
		Family:                  family,
		Model:                   resolved.WireModel,
		Effort:                  parsedEffort,
		WireEffort:              parsedWireEffort,
		Context:                 resolved.Context,
		MaxOutputTokens:         resolved.MaxOutputTokens,
		Thinking:                resolved.Thinking,
		ThinkingBudgetTokens:    resolved.ThinkingBudgetTokens,
		Instructions:            resolved.Instructions,
		Efforts:                 resolved.Efforts,
		Alias:                   resolved.Alias,
		SupportsTools:           resolved.SupportsTools,
		SupportsVision:          resolved.SupportsVision,
		ToolsCapability:         resolved.ToolsCapability,
		VisionCapability:        resolved.VisionCapability,
		ThinkingModes:           resolved.ThinkingModes,
		PassthroughOverrideName: resolved.PassthroughOverride,
		PassthroughOverride:     resolved.PassthroughConfig,
		OpenAICompatPassthrough: resolved.OpenAICompatPassthrough,
		TransportLimits:         maps.Clone(resolved.TransportLimits),
		WireProfile:             resolved.WireProfile,
		Pricing:                 resolved.Pricing,
	}, nil
}
