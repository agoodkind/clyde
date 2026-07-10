package resolver

import (
	"errors"
	"fmt"
	"log/slog"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog/correlation"
)

// ModelRegistry is the narrow interface the resolver consumes from the
// adapter's existing model registry. It exists so the resolver can be
// tested in isolation without dragging the full Registry implementation
// into every test.
//
// The single method mirrors model.Registry.Resolve. It accepts a model
// alias and a wire-form effort string (which may be empty) and returns
// the resolved model fields the resolver lifts into its typed view.
type ModelRegistry interface {
	Resolve(surface IngressSurface, alias, reqEffort string) (ResolvedModelView, error)
}

// IngressSurface is the typed model-routing surface.
type IngressSurface = adaptermodel.IngressSurface

const (
	// IngressCursor identifies Cursor's OpenAI-compatible listener.
	IngressCursor = adaptermodel.IngressCursor
	// IngressOpenAI identifies generic OpenAI-compatible ingress.
	IngressOpenAI = adaptermodel.IngressOpenAI
	// IngressAnthropic identifies native Anthropic Messages ingress.
	IngressAnthropic = adaptermodel.IngressAnthropic
)

// ResolvedModelView is the resolver's view of the existing model
// registry's per-alias output. It is a typed surface, not a pointer to
// the existing model.ResolvedAlias, so the resolver can be exercised
// with fakes that do not depend on the live registry.
type ResolvedModelView struct {
	Provider        ProviderID
	Family          string
	Model           string
	Effort          Effort
	Context         int
	MaxOutputTokens int
	// Thinking is the resolved thinking mode for this alias as a typed
	// string ("enabled", "adaptive", "disabled", or empty when not
	// applicable). The resolver carries it through so per-provider
	// dispatch can populate the upstream wire request without reaching
	// back into the model registry. Empty means leave the upstream
	// thinking field unset.
	Thinking string
	// ThinkingBudgetTokens is the explicit budget from the selected thinking
	// profile. A zero value means the resolved mode does not use a budget.
	ThinkingBudgetTokens int
	// Instructions carries any provider-neutral model instructions the
	// registry resolved for this alias. Empty means leave provider-native
	// instruction fields unchanged unless the caller supplied its own
	// system or developer content.
	Instructions string
	// Efforts is the list of allowed effort tiers the family declared.
	// Per-provider request builders gate output_config on this being
	// non-empty so they only send the effort field to families that
	// accept it. Empty means leave output_config unset.
	Efforts []string
	// Alias is the public alias the client sent, before normalization.
	Alias string
	// SupportsTools mirrors the family's declared tool capability.
	SupportsTools bool
	// SupportsVision mirrors the family's declared vision capability.
	SupportsVision bool
	// ToolsCapability is nil when a wildcard route has no trustworthy claim.
	ToolsCapability *bool
	// VisionCapability is nil when a wildcard route has no trustworthy claim.
	VisionCapability *bool
	// ObservedContext is the provider-specific context window the
	// registry resolved for capability reports. Zero means use Context.
	ObservedContext int
	// ThinkingModes enumerates the allowed thinking values the family
	// declared. Distinct from Thinking, which is the single bound mode.
	ThinkingModes []string
	// PassthroughOverrideName names an entry in the adapter's passthrough
	// override table. Set only for a named passthrough_override alias.
	PassthroughOverrideName string
	// PassthroughOverride is the immutable named override snapshot selected
	// with the exact alias. Dispatch must not reload it from a newer registry.
	PassthroughOverride config.AdapterPassthroughOverride
	// OpenAICompatPassthrough carries the inline configured upstream the
	// registry resolved for an unnamed passthrough_override alias.
	OpenAICompatPassthrough config.AdapterOpenAICompatPassthrough
	// TransportLimits carries profile-specific effective context limits.
	TransportLimits map[config.AdapterModelTransport]int
	// WireProfile identifies an exact learned Anthropic wire baseline.
	WireProfile string
	// Pricing carries the exact model's configured local rates.
	Pricing config.AdapterModelPricing
}

// ErrUnresolvedProvider signals that the model alias resolved to a
// backend the resolver does not support (today: passthrough_override or fallback).
// The dispatcher must fall back to its legacy path or reject the
// request explicitly.
var ErrUnresolvedProvider = errors.New("resolver: alias does not map to a known provider")

// Resolve consumes a typed cursor.Request and a ModelRegistry and
// returns a ResolvedRequest. It is a pure function in the sense that
// it does not perform IO; the registry is consulted in-memory.
//
// This is the Step A skeleton. The full implementation lands in Step C
// once the existing model registry exposes the ModelRegistry interface
// or the resolver gains a small adapter.
func Resolve(surface IngressSurface, req adaptercursor.Request, registry ModelRegistry) (ResolvedRequest, error) {
	if registry == nil {
		return ResolvedRequest{}, errors.New("resolver: nil registry")
	}
	view, err := registry.Resolve(surface, req.OpenAI.Model, req.OpenAI.ReasoningEffort)
	if err != nil {
		slog.Warn("adapter.resolver.resolve_failed", "concern", "adapter.models.resolve", "model", req.OpenAI.Model, "err", err)
		return ResolvedRequest{}, fmt.Errorf("resolve request model %s: %w", req.OpenAI.Model, err)
	}
	if !view.Provider.Valid() {
		return ResolvedRequest{}, ErrUnresolvedProvider
	}
	return ResolvedRequest{
		Provider: view.Provider,
		Family:   view.Family,
		Model:    view.Model,
		Effort:   view.Effort,
		ContextBudget: ContextBudget{
			InputTokens:  view.Context,
			OutputTokens: view.MaxOutputTokens,
			TotalTokens:  view.Context,
		},
		Thinking:                view.Thinking,
		ThinkingBudgetTokens:    view.ThinkingBudgetTokens,
		Instructions:            view.Instructions,
		Efforts:                 view.Efforts,
		Alias:                   view.Alias,
		SupportsTools:           view.SupportsTools,
		SupportsVision:          view.SupportsVision,
		ToolsCapability:         view.ToolsCapability,
		VisionCapability:        view.VisionCapability,
		ObservedContext:         view.ObservedContext,
		ThinkingModes:           view.ThinkingModes,
		MaxOutputTokens:         view.MaxOutputTokens,
		PassthroughOverrideName: view.PassthroughOverrideName,
		PassthroughOverride:     view.PassthroughOverride,
		OpenAICompatPassthrough: view.OpenAICompatPassthrough,
		TransportLimits:         view.TransportLimits,
		WireProfile:             view.WireProfile,
		Pricing:                 view.Pricing,
		Cursor:                  req,
		OpenAI:                  req.OpenAI,
		Verbosity:               "",
		RequestID:               "",
		Correlation:             correlation.Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	}, nil
}
