package config

import (
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
)

// AdapterModelProvider is a provider selected by an exact model or route.
type AdapterModelProvider string

const (
	// AdapterModelProviderCodex selects the direct Codex provider.
	AdapterModelProviderCodex AdapterModelProvider = "codex"
	// AdapterModelProviderAnthropic selects the direct Anthropic provider.
	AdapterModelProviderAnthropic AdapterModelProvider = "anthropic"
	// AdapterModelProviderPassthroughOverride selects a named OpenAI-compatible upstream.
	AdapterModelProviderPassthroughOverride AdapterModelProvider = "passthrough_override"
)

// AdapterIngressSurface identifies the request envelope used for routing.
type AdapterIngressSurface string

const (
	// AdapterIngressCursor is OpenAI-shaped traffic from the Cursor listener.
	AdapterIngressCursor AdapterIngressSurface = "cursor"
	// AdapterIngressOpenAI is generic OpenAI-compatible ingress traffic.
	AdapterIngressOpenAI AdapterIngressSurface = "openai"
	// AdapterIngressAnthropic is native Anthropic Messages ingress traffic.
	AdapterIngressAnthropic AdapterIngressSurface = "anthropic"
)

// AdapterReasoningEffort is a provider-owned reasoning tier string.
type AdapterReasoningEffort string

// AdapterModelTransport identifies a provider transport with its own context limit.
type AdapterModelTransport string

const (
	// AdapterModelTransportCodexHTTP is the Codex Responses HTTP transport.
	AdapterModelTransportCodexHTTP AdapterModelTransport = "codex_http"
	// AdapterModelTransportCodexWebsocket is the Codex Responses websocket transport.
	AdapterModelTransportCodexWebsocket AdapterModelTransport = "codex_websocket"
	// AdapterModelTransportAnthropic is the Anthropic Messages transport.
	AdapterModelTransportAnthropic AdapterModelTransport = "anthropic"
)

// AdapterThinkingMode is the provider wire grammar for a thinking profile.
type AdapterThinkingMode string

const (
	// AdapterThinkingModeDisabled disables provider reasoning blocks.
	AdapterThinkingModeDisabled AdapterThinkingMode = "disabled"
	// AdapterThinkingModeEnabled sends an explicit thinking token budget.
	AdapterThinkingModeEnabled AdapterThinkingMode = "enabled"
	// AdapterThinkingModeAdaptive delegates the budget to the provider.
	AdapterThinkingModeAdaptive AdapterThinkingMode = "adaptive"
)

// AdapterWireModelPolicy controls how wildcard routes choose the wire model.
type AdapterWireModelPolicy string

const (
	// AdapterWireModelPolicyPreserve forwards the requested model unchanged.
	AdapterWireModelPolicyPreserve AdapterWireModelPolicy = "preserve"
)

// AdapterWildcardCapabilityPolicy controls undeclared model capability checks.
type AdapterWildcardCapabilityPolicy string

const (
	// AdapterWildcardCapabilityPolicyPassthrough leaves unknown capabilities unset.
	AdapterWildcardCapabilityPolicyPassthrough AdapterWildcardCapabilityPolicy = "passthrough"
)

// AdapterGeneratedAliasDimension is one profile axis used to generate aliases.
type AdapterGeneratedAliasDimension string

const (
	// AdapterGeneratedAliasDimensionContext expands profile context variants.
	AdapterGeneratedAliasDimensionContext AdapterGeneratedAliasDimension = "context"
	// AdapterGeneratedAliasDimensionReasoningEffort expands reasoning effort values.
	AdapterGeneratedAliasDimensionReasoningEffort AdapterGeneratedAliasDimension = "reasoning_effort"
	// AdapterGeneratedAliasDimensionThinkingProfile expands named thinking profiles.
	AdapterGeneratedAliasDimensionThinkingProfile AdapterGeneratedAliasDimension = "thinking_profile"
)

// AdapterModelProfile declares reusable exact-model capabilities.
type AdapterModelProfile struct {
	Contexts                  []AdapterModelProfileContext                      `json:"contexts,omitempty" toml:"contexts,omitempty"`
	MaxOutputTokens           int                                               `json:"maxOutputTokens,omitempty" toml:"max_output_tokens,omitempty"`
	ReasoningEfforts          []AdapterReasoningEffort                          `json:"reasoningEfforts,omitempty" toml:"reasoning_efforts,omitempty"`
	ReasoningEffortWireValues map[AdapterReasoningEffort]AdapterReasoningEffort `json:"reasoningEffortWireValues,omitempty" toml:"reasoning_effort_wire_values,omitempty"`
	DefaultEffort             AdapterReasoningEffort                            `json:"defaultEffort,omitempty" toml:"default_effort,omitempty"`
	SupportsTools             *bool                                             `json:"supportsTools,omitempty" toml:"supports_tools,omitempty"`
	SupportsVision            *bool                                             `json:"supportsVision,omitempty" toml:"supports_vision,omitempty"`
	ThinkingProfiles          map[string]AdapterModelThinkingProfile            `json:"thinkingProfiles,omitempty" toml:"thinking_profiles,omitempty"`
}

// AdapterModelProfileContext declares one nominal context variant.
type AdapterModelProfileContext struct {
	Name            string                       `json:"name,omitempty" toml:"name,omitempty"`
	Tokens          int                          `json:"tokens,omitempty" toml:"tokens,omitempty"`
	AliasSuffix     string                       `json:"aliasSuffix,omitempty" toml:"alias_suffix,omitempty"`
	WireSuffix      string                       `json:"wireSuffix,omitempty" toml:"wire_suffix,omitempty"`
	TransportLimits []AdapterModelTransportLimit `json:"transportLimits,omitempty" toml:"transport_limits,omitempty"`
}

// AdapterModelTransportLimit overrides one context variant for a transport.
type AdapterModelTransportLimit struct {
	Transport AdapterModelTransport `json:"transport,omitempty" toml:"transport,omitempty"`
	Tokens    int                   `json:"tokens,omitempty" toml:"tokens,omitempty"`
}

// AdapterModelThinkingProfile declares a named provider thinking shape.
type AdapterModelThinkingProfile struct {
	Mode         AdapterThinkingMode `json:"mode,omitempty" toml:"mode,omitempty"`
	BudgetTokens int                 `json:"budgetTokens,omitempty" toml:"budget_tokens,omitempty"`
}

// AdapterModelDeclaration binds one canonical exact request ID to a provider.
type AdapterModelDeclaration struct {
	Provider            AdapterModelProvider         `json:"provider,omitempty" toml:"provider,omitempty"`
	WireModel           string                       `json:"wireModel,omitempty" toml:"wire_model,omitempty"`
	Profile             string                       `json:"profile,omitempty" toml:"profile,omitempty"`
	InstructionsFile    string                       `json:"instructionsFile,omitempty" toml:"instructions_file,omitempty"`
	Instructions        string                       `json:"-" toml:"-"`
	Pricing             AdapterModelPricing          `json:"pricing,omitzero" toml:"pricing,omitempty"`
	Aliases             []AdapterModelAlias          `json:"aliases,omitempty" toml:"aliases,omitempty"`
	Advertise           bool                         `json:"advertise,omitempty" toml:"advertise,omitempty"`
	GeneratedAliases    AdapterModelGeneratedAliases `json:"generatedAliases,omitzero" toml:"generated_aliases,omitempty"`
	PassthroughOverride string                       `json:"passthroughOverride,omitempty" toml:"passthrough_override,omitempty"`
	WireProfile         string                       `json:"wireProfile,omitempty" toml:"wire_profile,omitempty"`
}

// AdapterModelAlias declares one exact alias and optional profile bindings.
type AdapterModelAlias struct {
	ID              string                 `json:"id,omitempty" toml:"id,omitempty"`
	Advertise       bool                   `json:"advertise,omitempty" toml:"advertise,omitempty"`
	ReasoningEffort AdapterReasoningEffort `json:"reasoningEffort,omitempty" toml:"reasoning_effort,omitempty"`
	Context         string                 `json:"context,omitempty" toml:"context,omitempty"`
	ThinkingProfile string                 `json:"thinkingProfile,omitempty" toml:"thinking_profile,omitempty"`
}

// AdapterModelGeneratedAliases declares configuration-driven alias expansion.
type AdapterModelGeneratedAliases struct {
	Prefix     string                           `json:"prefix,omitempty" toml:"prefix,omitempty"`
	Advertise  bool                             `json:"advertise,omitempty" toml:"advertise,omitempty"`
	Dimensions []AdapterGeneratedAliasDimension `json:"dimensions,omitempty" toml:"dimensions,omitempty"`
}

// AdapterModelRoute is one ordered full-string wildcard provider claim.
type AdapterModelRoute struct {
	Match            string                          `json:"match,omitempty" toml:"match,omitempty"`
	Surfaces         []AdapterIngressSurface         `json:"surfaces,omitempty" toml:"surfaces,omitempty"`
	Provider         AdapterModelProvider            `json:"provider,omitempty" toml:"provider,omitempty"`
	WireModelPolicy  AdapterWireModelPolicy          `json:"wireModelPolicy,omitempty" toml:"wire_model_policy,omitempty"`
	CapabilityPolicy AdapterWildcardCapabilityPolicy `json:"capabilityPolicy,omitempty" toml:"capability_policy,omitempty"`
}

// ModelPricing returns model-local rates keyed by configured wire model.
func (adapter AdapterConfig) ModelPricing() map[string]AdapterModelPricing {
	pricing := make(map[string]AdapterModelPricing, len(adapter.Models))
	for _, declaration := range adapter.Models {
		wireModel := strings.TrimSpace(declaration.WireModel)
		if wireModel == "" || isZeroAdapterModelPricing(declaration.Pricing) {
			continue
		}
		pricing[wireModel] = declaration.Pricing
	}
	return pricing
}

func pruneEmptyModelDeclarations(adapter *AdapterConfig) {
	if adapter == nil {
		return
	}
	for canonicalID, model := range adapter.Models {
		if isEmptyModelDeclaration(model) {
			delete(adapter.Models, canonicalID)
		}
	}
}

func isEmptyModelDeclaration(model AdapterModelDeclaration) bool {
	return strings.TrimSpace(string(model.Provider)) == "" &&
		strings.TrimSpace(model.WireModel) == "" &&
		strings.TrimSpace(model.Profile) == "" &&
		strings.TrimSpace(model.InstructionsFile) == "" &&
		isZeroAdapterModelPricing(model.Pricing) &&
		len(model.Aliases) == 0 &&
		!model.Advertise &&
		strings.TrimSpace(model.GeneratedAliases.Prefix) == "" &&
		!model.GeneratedAliases.Advertise &&
		len(model.GeneratedAliases.Dimensions) == 0 &&
		strings.TrimSpace(model.PassthroughOverride) == "" &&
		strings.TrimSpace(model.WireProfile) == ""
}

func isZeroAdapterModelPricing(pricing AdapterModelPricing) bool {
	var zero AdapterModelPricing
	return pricing == zero
}

func validateAdapterModelCatalog(adapter *AdapterConfig) error {
	if adapter == nil {
		return nil
	}
	for name, profile := range adapter.ModelProfiles {
		if err := validateAdapterModelProfile(name, profile); err != nil {
			return err
		}
	}
	aliases, err := validateAdapterModelDeclarations(adapter)
	if err != nil {
		return err
	}
	if err := validateAdapterDefaultModel(*adapter, aliases); err != nil {
		return err
	}
	for index, route := range adapter.ModelRoutes {
		if err := validateAdapterModelRoute(*adapter, index, route); err != nil {
			return err
		}
	}
	return nil
}

func validateAdapterModelProfile(name string, profile AdapterModelProfile) error {
	modelPath := "adapter.model_profiles." + name
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("adapter.model_profiles contains an empty profile name")
	}
	if len(profile.Contexts) == 0 {
		return fmt.Errorf("%s.contexts must contain at least one context", modelPath)
	}
	if err := validateAdapterProfileContexts(modelPath, profile.Contexts); err != nil {
		return err
	}
	if profile.MaxOutputTokens <= 0 {
		return fmt.Errorf("%s.max_output_tokens must be positive", modelPath)
	}
	if profile.SupportsTools == nil {
		return fmt.Errorf("%s.supports_tools must be set", modelPath)
	}
	if profile.SupportsVision == nil {
		return fmt.Errorf("%s.supports_vision must be set", modelPath)
	}
	efforts := make(map[AdapterReasoningEffort]bool, len(profile.ReasoningEfforts))
	for _, effort := range profile.ReasoningEfforts {
		if strings.TrimSpace(string(effort)) == "" {
			return fmt.Errorf("%s.reasoning_efforts contains an empty effort", modelPath)
		}
		if efforts[effort] {
			return fmt.Errorf("%s.reasoning_efforts contains duplicate reasoning effort %q", modelPath, effort)
		}
		efforts[effort] = true
	}
	if profile.DefaultEffort != "" && !efforts[profile.DefaultEffort] {
		return fmt.Errorf("%s.default_effort %q is not in reasoning_efforts", modelPath, profile.DefaultEffort)
	}
	for effort, wireValue := range profile.ReasoningEffortWireValues {
		if !efforts[effort] {
			return fmt.Errorf(
				"%s.reasoning_effort_wire_values key %q is not in reasoning_efforts",
				modelPath,
				effort,
			)
		}
		if strings.TrimSpace(string(wireValue)) == "" {
			return fmt.Errorf(
				"%s.reasoning_effort_wire_values.%s must be nonempty",
				modelPath,
				effort,
			)
		}
	}
	for name, thinking := range profile.ThinkingProfiles {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s.thinking_profiles contains an empty name", modelPath)
		}
		if err := validateAdapterThinkingProfile(
			modelPath+".thinking_profiles."+name,
			thinking,
			profile.MaxOutputTokens,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateAdapterProfileContexts(modelPath string, contexts []AdapterModelProfileContext) error {
	contextNames := make(map[string]bool, len(contexts))
	for index, contextProfile := range contexts {
		contextPath := fmt.Sprintf("%s.contexts[%d]", modelPath, index)
		contextName := strings.TrimSpace(contextProfile.Name)
		if contextName == "" {
			return fmt.Errorf("%s.name must be set", contextPath)
		}
		if contextNames[contextName] {
			return fmt.Errorf("%s.contexts contains duplicate context %q", modelPath, contextName)
		}
		contextNames[contextName] = true
		if contextProfile.Tokens <= 0 {
			return fmt.Errorf("%s.tokens must be positive", contextPath)
		}
		if err := validateAdapterTransportLimits(contextPath, contextProfile.TransportLimits); err != nil {
			return err
		}
	}
	return nil
}

func validateAdapterTransportLimits(contextPath string, limits []AdapterModelTransportLimit) error {
	transports := make(map[AdapterModelTransport]bool, len(limits))
	for index, limit := range limits {
		limitPath := fmt.Sprintf("%s.transport_limits[%d]", contextPath, index)
		if !limit.Transport.valid() {
			return fmt.Errorf("%s has unsupported transport %q", limitPath, limit.Transport)
		}
		if transports[limit.Transport] {
			return fmt.Errorf("%s contains duplicate transport %q", contextPath, limit.Transport)
		}
		transports[limit.Transport] = true
		if limit.Tokens <= 0 {
			return fmt.Errorf("%s.tokens must be positive", limitPath)
		}
	}
	return nil
}

func validateAdapterThinkingProfile(
	profilePath string,
	profile AdapterModelThinkingProfile,
	maxOutputTokens int,
) error {
	switch profile.Mode {
	case AdapterThinkingModeDisabled, AdapterThinkingModeAdaptive:
		if profile.BudgetTokens != 0 {
			return fmt.Errorf("%s.budget_tokens must be zero for mode %q", profilePath, profile.Mode)
		}
	case AdapterThinkingModeEnabled:
		if profile.BudgetTokens <= 0 {
			return fmt.Errorf("%s.budget_tokens must be positive for enabled mode", profilePath)
		}
		if profile.BudgetTokens >= maxOutputTokens {
			return fmt.Errorf("%s.budget_tokens must be less than max_output_tokens", profilePath)
		}
	default:
		return fmt.Errorf("%s.mode %q is invalid", profilePath, profile.Mode)
	}
	return nil
}

func validateAdapterModelDeclarations(adapter *AdapterConfig) (map[string]AdapterModelProvider, error) {
	aliases := make(map[string]AdapterModelProvider, len(adapter.Models))
	canonicalIDs := make([]string, 0, len(adapter.Models))
	for canonicalID := range adapter.Models {
		canonicalIDs = append(canonicalIDs, canonicalID)
	}
	slices.Sort(canonicalIDs)
	for _, canonicalID := range canonicalIDs {
		id := strings.TrimSpace(canonicalID)
		if id == "" {
			return nil, fmt.Errorf("adapter.models contains an empty model id")
		}
		key := strings.ToLower(id)
		if _, exists := aliases[key]; exists {
			return nil, fmt.Errorf("adapter.models contains duplicate exact alias %q", id)
		}
		aliases[key] = adapter.Models[canonicalID].Provider
	}
	pricingModels := make(map[string]string, len(adapter.Models))
	pricingValues := make(map[string]AdapterModelPricing, len(adapter.Models))
	for _, canonicalID := range canonicalIDs {
		model := adapter.Models[canonicalID]
		profile, ok := adapter.ModelProfiles[strings.TrimSpace(model.Profile)]
		if strings.TrimSpace(model.Profile) == "" || !ok {
			return nil, fmt.Errorf("adapter.models.%s.profile %q does not reference adapter.model_profiles", canonicalID, model.Profile)
		}
		if err := validateAdapterModelDeclaration(*adapter, canonicalID, model, profile, aliases); err != nil {
			return nil, err
		}
		if isZeroAdapterModelPricing(model.Pricing) {
			continue
		}
		wireModel := strings.ToLower(strings.TrimSpace(model.WireModel))
		if previousPricing, exists := pricingValues[wireModel]; exists {
			if previousPricing != model.Pricing {
				return nil, fmt.Errorf(
					"adapter.models.%s and adapter.models.%s declare conflicting pricing for wire_model %q",
					pricingModels[wireModel],
					canonicalID,
					wireModel,
				)
			}
			continue
		}
		pricingModels[wireModel] = canonicalID
		pricingValues[wireModel] = model.Pricing
	}
	return aliases, nil
}

func validateAdapterModelDeclaration(adapter AdapterConfig, canonicalID string, model AdapterModelDeclaration, profile AdapterModelProfile, aliases map[string]AdapterModelProvider) error {
	modelPath := "adapter.models." + canonicalID
	if !model.Provider.valid() {
		return fmt.Errorf("%s.provider %q is invalid", modelPath, model.Provider)
	}
	if strings.TrimSpace(model.WireModel) == "" {
		return fmt.Errorf("%s.wire_model must be set", modelPath)
	}
	if model.Pricing.InputPerMTok < 0 || model.Pricing.OutputPerMTok < 0 ||
		model.Pricing.CacheWritePerMTok < 0 || model.Pricing.CacheReadPerMTok < 0 {
		return fmt.Errorf("%s.pricing values must be non-negative", modelPath)
	}
	providerEnabled := adapterProviderEnabled(adapter, model.Provider)
	if !providerEnabled && model.Advertise {
		return fmt.Errorf("%s advertises a disabled provider %q", modelPath, model.Provider)
	}
	if !providerEnabled && model.GeneratedAliases.Advertise {
		return fmt.Errorf("%s.generated_aliases advertises a disabled provider %q", modelPath, model.Provider)
	}
	if model.Provider == AdapterModelProviderPassthroughOverride {
		name := strings.TrimSpace(model.PassthroughOverride)
		if name == "" {
			return fmt.Errorf("%s.passthrough_override must be set", modelPath)
		}
		if _, ok := adapter.PassthroughOverrides[name]; !ok {
			return fmt.Errorf("%s.passthrough_override %q is not declared", modelPath, name)
		}
	} else if strings.TrimSpace(model.PassthroughOverride) != "" {
		return fmt.Errorf("%s.passthrough_override is only valid for provider passthrough_override", modelPath)
	}
	if strings.TrimSpace(model.WireProfile) != "" && model.Provider != AdapterModelProviderAnthropic {
		return fmt.Errorf("%s.wire_profile is only valid for provider anthropic", modelPath)
	}
	for index, alias := range model.Aliases {
		aliasPath := fmt.Sprintf("%s.aliases[%d]", modelPath, index)
		id := strings.TrimSpace(alias.ID)
		if id == "" {
			return fmt.Errorf("%s.id must be set", aliasPath)
		}
		key := strings.ToLower(id)
		if _, exists := aliases[key]; exists {
			return fmt.Errorf("%s.id %q is a duplicate exact alias", aliasPath, id)
		}
		aliases[key] = model.Provider
		if alias.Advertise && !providerEnabled {
			return fmt.Errorf("%s advertises a disabled provider %q", aliasPath, model.Provider)
		}
		if err := validateAdapterModelAliasReferences(aliasPath, alias, profile); err != nil {
			return err
		}
	}
	return validateAdapterGeneratedAliases(modelPath, model.GeneratedAliases, profile)
}

func validateAdapterModelAliasReferences(aliasPath string, alias AdapterModelAlias, profile AdapterModelProfile) error {
	if alias.ReasoningEffort != "" && !containsAdapterEffort(profile.ReasoningEfforts, alias.ReasoningEffort) {
		return fmt.Errorf("%s.reasoning_effort %q is not declared by the profile", aliasPath, alias.ReasoningEffort)
	}
	if alias.Context != "" && !containsAdapterContext(profile.Contexts, alias.Context) {
		return fmt.Errorf("%s.context %q is not declared by the profile", aliasPath, alias.Context)
	}
	if alias.ThinkingProfile != "" {
		if _, ok := profile.ThinkingProfiles[alias.ThinkingProfile]; !ok {
			return fmt.Errorf("%s.thinking_profile %q is not declared by the profile", aliasPath, alias.ThinkingProfile)
		}
	}
	return nil
}

func validateAdapterGeneratedAliases(modelPath string, generated AdapterModelGeneratedAliases, profile AdapterModelProfile) error {
	if len(generated.Dimensions) == 0 {
		if strings.TrimSpace(generated.Prefix) != "" || generated.Advertise {
			return fmt.Errorf("%s.generated_aliases.dimensions must be set when generated aliases are configured", modelPath)
		}
		return nil
	}
	if strings.TrimSpace(generated.Prefix) == "" {
		return fmt.Errorf("%s.generated_aliases.prefix must be set", modelPath)
	}
	hasEffortDimension := false
	seen := make(map[AdapterGeneratedAliasDimension]bool, len(generated.Dimensions))
	for _, dimension := range generated.Dimensions {
		switch dimension {
		case AdapterGeneratedAliasDimensionContext:
		case AdapterGeneratedAliasDimensionReasoningEffort:
			hasEffortDimension = true
		case AdapterGeneratedAliasDimensionThinkingProfile:
			if len(profile.ThinkingProfiles) == 0 {
				return fmt.Errorf("%s.generated_aliases requires profile thinking_profiles", modelPath)
			}
		default:
			return fmt.Errorf("%s.generated_aliases contains invalid dimension %q", modelPath, dimension)
		}
		if seen[dimension] {
			return fmt.Errorf("%s.generated_aliases contains duplicate dimension %q", modelPath, dimension)
		}
		seen[dimension] = true
	}
	if generated.Advertise && !hasEffortDimension && profile.DefaultEffort == "" {
		return fmt.Errorf("%s.generated_aliases requires profile default_effort for an advertised bare alias", modelPath)
	}
	return nil
}

func validateAdapterDefaultModel(adapter AdapterConfig, aliases map[string]AdapterModelProvider) error {
	defaultModel := strings.TrimSpace(adapter.DefaultModel)
	if defaultModel == "" {
		return nil
	}
	provider, ok := aliases[strings.ToLower(defaultModel)]
	if !ok {
		return fmt.Errorf("adapter.default_model %q is not an exact model or alias", adapter.DefaultModel)
	}
	if !adapterProviderEnabled(adapter, provider) {
		return fmt.Errorf("adapter.default_model %q selects disabled provider %q", adapter.DefaultModel, provider)
	}
	return nil
}

func validateAdapterModelRoute(adapter AdapterConfig, index int, route AdapterModelRoute) error {
	routePath := fmt.Sprintf("adapter.model_routes[%d]", index)
	if strings.TrimSpace(route.Match) == "" {
		return fmt.Errorf("%s.match must be set", routePath)
	}
	if _, err := path.Match(route.Match, "model"); err != nil {
		wrapped := fmt.Errorf("%s.match is an invalid glob: %w", routePath, err)
		slog.Warn(
			"config.adapter.model_route_glob_invalid",
			"concern", "config",
			"route_index", index,
			"pattern", route.Match,
			"err", wrapped,
		)
		return wrapped
	}
	if len(route.Surfaces) == 0 {
		return fmt.Errorf("%s.surfaces must contain at least one surface", routePath)
	}
	surfaces := make(map[AdapterIngressSurface]bool, len(route.Surfaces))
	for _, surface := range route.Surfaces {
		if !surface.valid() {
			return fmt.Errorf("%s has invalid surface %q", routePath, surface)
		}
		if surfaces[surface] {
			return fmt.Errorf("%s contains duplicate surface %q", routePath, surface)
		}
		surfaces[surface] = true
	}
	if route.Provider != AdapterModelProviderCodex && route.Provider != AdapterModelProviderAnthropic {
		return fmt.Errorf("%s.provider %q is invalid for wildcard routing", routePath, route.Provider)
	}
	if !adapterProviderEnabled(adapter, route.Provider) {
		return fmt.Errorf("%s selects disabled provider %q", routePath, route.Provider)
	}
	if route.WireModelPolicy != AdapterWireModelPolicyPreserve {
		return fmt.Errorf("%s.wire_model_policy must be preserve", routePath)
	}
	if route.CapabilityPolicy != AdapterWildcardCapabilityPolicyPassthrough {
		return fmt.Errorf("%s.capability_policy must be passthrough", routePath)
	}
	return nil
}

func adapterProviderEnabled(adapter AdapterConfig, provider AdapterModelProvider) bool {
	switch provider {
	case AdapterModelProviderCodex:
		return adapter.Codex.Enabled
	case AdapterModelProviderAnthropic:
		return adapter.Anthropic.Enabled || adapter.DirectOAuth
	case AdapterModelProviderPassthroughOverride:
		return true
	default:
		return false
	}
}

func (provider AdapterModelProvider) valid() bool {
	switch provider {
	case AdapterModelProviderCodex, AdapterModelProviderAnthropic, AdapterModelProviderPassthroughOverride:
		return true
	default:
		return false
	}
}

func (surface AdapterIngressSurface) valid() bool {
	switch surface {
	case AdapterIngressCursor, AdapterIngressOpenAI, AdapterIngressAnthropic:
		return true
	default:
		return false
	}
}

func (transport AdapterModelTransport) valid() bool {
	switch transport {
	case AdapterModelTransportCodexHTTP, AdapterModelTransportCodexWebsocket, AdapterModelTransportAnthropic:
		return true
	default:
		return false
	}
}

func containsAdapterEffort(efforts []AdapterReasoningEffort, want AdapterReasoningEffort) bool {
	return slices.Contains(efforts, want)
}

func containsAdapterContext(contexts []AdapterModelProfileContext, want string) bool {
	for _, contextProfile := range contexts {
		if contextProfile.Name == want {
			return true
		}
	}
	return false
}
