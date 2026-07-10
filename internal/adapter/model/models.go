package model

import (
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/config"
)

// BackendID identifies the provider that fulfils a resolved request.
type BackendID string

// String returns the provider's configured wire value.
func (backend BackendID) String() string { return string(backend) }

// Valid reports whether the adapter has a dispatch path for the provider.
func (backend BackendID) Valid() bool {
	switch backend {
	case BackendClaude, BackendAnthropic, BackendCodex, BackendPassthroughOverride:
		return true
	default:
		return false
	}
}

const (
	// BackendClaude is the retained legacy subprocess provider identity.
	BackendClaude BackendID = "claude"
	// BackendAnthropic selects the direct Anthropic Messages provider.
	BackendAnthropic BackendID = "anthropic"
	// BackendCodex selects the direct Codex Responses provider.
	BackendCodex BackendID = "codex"
	// BackendPassthroughOverride selects an OpenAI-compatible upstream.
	BackendPassthroughOverride BackendID = "passthrough_override"
)

const (
	// EffortLow is retained for callers that use the established constants.
	EffortLow = "low"
	// EffortMedium is retained for callers that use the established constants.
	EffortMedium = "medium"
	// EffortHigh is retained for callers that use the established constants.
	EffortHigh = "high"
	// EffortXHigh is retained for callers that use the established constants.
	EffortXHigh = "xhigh"
	// EffortMax is retained for callers that use the established constants.
	EffortMax = "max"
)

const (
	// ThinkingDefault leaves provider thinking configuration unset.
	ThinkingDefault = "default"
	// ThinkingAdaptive selects Anthropic adaptive thinking.
	ThinkingAdaptive = "adaptive"
	// ThinkingEnabled selects explicitly budgeted thinking.
	ThinkingEnabled = "enabled"
	// ThinkingDisabled disables thinking.
	ThinkingDisabled = "disabled"
)

// IngressSurface is the typed routing surface supplied by the HTTP boundary.
type IngressSurface = config.AdapterIngressSurface

const (
	// IngressCursor identifies Cursor's OpenAI-shaped listener.
	IngressCursor = config.AdapterIngressCursor
	// IngressOpenAI identifies generic OpenAI-compatible ingress.
	IngressOpenAI = config.AdapterIngressOpenAI
	// IngressAnthropic identifies native Anthropic Messages ingress.
	IngressAnthropic = config.AdapterIngressAnthropic
)

// ResolvedAlias is one canonical catalog record projected for a request alias.
// Exact records populate capabilities from their profile. Wildcard and global
// passthrough records leave optional capability pointers nil.
type ResolvedAlias struct {
	Alias                   string
	Backend                 BackendID
	WireModel               string
	Profile                 string
	Instructions            string
	Context                 int
	TransportLimits         map[config.AdapterModelTransport]int
	Efforts                 []string
	EffortWireValues        map[string]string
	DefaultEffort           string
	Effort                  string
	WireEffort              string
	ThinkingModes           []string
	Thinking                string
	ThinkingBudgetTokens    int
	MaxOutputTokens         int
	ToolsCapability         *bool
	VisionCapability        *bool
	SupportsTools           bool
	SupportsVision          bool
	PassthroughOverride     string
	PassthroughConfig       config.AdapterPassthroughOverride
	OpenAICompatPassthrough config.AdapterOpenAICompatPassthrough
	Pricing                 config.AdapterModelPricing
	WireProfile             string
	Advertise               bool
}

type registryRecord struct {
	resolved        ResolvedAlias
	providerEnabled bool
	boundEffort     config.AdapterReasoningEffort
}

type routeRule struct {
	pattern         string
	surfaces        []IngressSurface
	provider        BackendID
	providerEnabled bool
	wireProfile     string
}

// Registry owns the exact catalog, ordered routes, and fallback transport.
type Registry struct {
	exact                map[string]registryRecord
	advertised           map[string]ResolvedAlias
	passthroughOverrides map[string]config.AdapterPassthroughOverride
	routes               []routeRule
	defaultModel         string
	openAICompat         config.AdapterOpenAICompatPassthrough
}

// NewRegistry builds one registry exclusively from the declarative model
// catalog. Legacy TOML keys are ignored by the config decoder and are never
// consulted here.
func NewRegistry(cfg config.AdapterConfig) (*Registry, error) {
	if err := validateAdapterCoreConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateAdapterLogprobs(cfg.Logprobs); err != nil {
		return nil, err
	}
	registry := &Registry{
		exact:                make(map[string]registryRecord),
		advertised:           make(map[string]ResolvedAlias),
		passthroughOverrides: make(map[string]config.AdapterPassthroughOverride, len(cfg.PassthroughOverrides)),
		routes:               make([]routeRule, 0, len(cfg.ModelRoutes)),
		defaultModel:         strings.TrimSpace(cfg.DefaultModel),
		openAICompat:         cfg.OpenAICompatPassthrough,
	}
	for name, passthrough := range cfg.PassthroughOverrides {
		registry.passthroughOverrides[strings.ToLower(strings.TrimSpace(name))] = passthrough
	}
	for _, canonicalID := range slices.Sorted(maps.Keys(cfg.Models)) {
		declaration := cfg.Models[canonicalID]
		profile, ok := cfg.ModelProfiles[strings.TrimSpace(declaration.Profile)]
		if !ok {
			return nil, fmt.Errorf("adapter: model %q references unknown profile %q", canonicalID, declaration.Profile)
		}
		if err := registry.addDeclaration(cfg, canonicalID, declaration, profile); err != nil {
			return nil, err
		}
	}
	if registry.defaultModel != "" {
		if _, ok := registry.exact[modelKey(registry.defaultModel)]; !ok {
			return nil, fmt.Errorf("adapter: default_model %q is not an exact model or alias", registry.defaultModel)
		}
	}
	for index, configured := range cfg.ModelRoutes {
		if _, err := path.Match(configured.Match, "model"); err != nil {
			wrapped := fmt.Errorf("adapter: model route %d has invalid glob %q: %w", index, configured.Match, err)
			modelResolveLog.Logger().Warn(
				"adapter.registry.route_glob_invalid",
				"route_index", index,
				"pattern", configured.Match,
				"err", wrapped,
			)
			return nil, wrapped
		}
		registry.routes = append(registry.routes, routeRule{
			pattern:         configured.Match,
			surfaces:        append([]IngressSurface(nil), configured.Surfaces...),
			provider:        backendForProvider(configured.Provider),
			providerEnabled: providerEnabled(cfg, configured.Provider),
			wireProfile:     defaultWireProfile(cfg, configured.Provider),
		})
	}
	modelCatalogLog.Logger().Info(
		"adapter.registry.catalog_loaded",
		"concern", "adapter.models.catalog",
		"models", len(cfg.Models),
		"aliases", len(registry.exact),
		"routes", len(registry.routes),
		"advertised", len(registry.advertised),
	)
	return registry, nil
}

func (registry *Registry) addDeclaration(
	cfg config.AdapterConfig,
	canonicalID string,
	declaration config.AdapterModelDeclaration,
	profile config.AdapterModelProfile,
) error {
	var canonicalBinding config.AdapterModelAlias
	base, err := resolvedFromDeclaration(canonicalID, declaration, profile, canonicalBinding)
	if err != nil {
		return err
	}
	providerIsEnabled := providerEnabled(cfg, declaration.Provider)
	if err := registry.addExact(canonicalID, base, providerIsEnabled, "", declaration.Advertise); err != nil {
		return err
	}
	for _, alias := range declaration.Aliases {
		resolved, resolveErr := resolvedFromDeclaration(alias.ID, declaration, profile, alias)
		if resolveErr != nil {
			return resolveErr
		}
		if err := registry.addExact(alias.ID, resolved, providerIsEnabled, alias.ReasoningEffort, alias.Advertise); err != nil {
			return err
		}
	}
	return registry.addGeneratedAliases(declaration, profile, providerIsEnabled)
}

func resolvedFromDeclaration(
	alias string,
	declaration config.AdapterModelDeclaration,
	profile config.AdapterModelProfile,
	binding config.AdapterModelAlias,
) (ResolvedAlias, error) {
	contextProfile, err := selectContext(profile.Contexts, binding.Context)
	if err != nil {
		wrapped := fmt.Errorf("adapter: model alias %q: %w", alias, err)
		modelResolveLog.Logger().Warn(
			"adapter.registry.alias_context_invalid",
			"alias", alias,
			"err", wrapped,
		)
		return ResolvedAlias{}, wrapped
	}
	thinking, thinkingBudgetTokens, thinkingModes, err := selectThinking(profile.ThinkingProfiles, binding.ThinkingProfile)
	if err != nil {
		wrapped := fmt.Errorf("adapter: model alias %q: %w", alias, err)
		modelResolveLog.Logger().Warn(
			"adapter.registry.alias_thinking_invalid",
			"alias", alias,
			"err", wrapped,
		)
		return ResolvedAlias{}, wrapped
	}
	efforts := make([]string, 0, len(profile.ReasoningEfforts))
	for _, effort := range profile.ReasoningEfforts {
		efforts = append(efforts, string(effort))
	}
	effortWireValues := make(map[string]string, len(profile.ReasoningEffortWireValues))
	for effort, wireValue := range profile.ReasoningEffortWireValues {
		effortWireValues[string(effort)] = string(wireValue)
	}
	transportLimits := make(map[config.AdapterModelTransport]int, len(contextProfile.TransportLimits))
	for _, limit := range contextProfile.TransportLimits {
		transportLimits[limit.Transport] = limit.Tokens
	}
	tools := cloneBool(profile.SupportsTools)
	vision := cloneBool(profile.SupportsVision)
	return ResolvedAlias{
		Alias:                strings.TrimSpace(alias),
		Backend:              backendForProvider(declaration.Provider),
		WireModel:            declaration.WireModel + contextProfile.WireSuffix,
		Profile:              declaration.Profile,
		Instructions:         declaration.Instructions,
		Context:              contextProfile.Tokens,
		TransportLimits:      transportLimits,
		Efforts:              efforts,
		EffortWireValues:     effortWireValues,
		DefaultEffort:        string(profile.DefaultEffort),
		Effort:               string(binding.ReasoningEffort),
		WireEffort:           "",
		ThinkingModes:        thinkingModes,
		Thinking:             thinking,
		ThinkingBudgetTokens: thinkingBudgetTokens,
		MaxOutputTokens:      profile.MaxOutputTokens,
		ToolsCapability:      tools,
		VisionCapability:     vision,
		SupportsTools:        tools != nil && *tools,
		SupportsVision:       vision != nil && *vision,
		PassthroughOverride:  declaration.PassthroughOverride,
		PassthroughConfig: config.AdapterPassthroughOverride{
			BaseURL:   "",
			APIKey:    "",
			APIKeyEnv: "",
			Model:     "",
		},
		OpenAICompatPassthrough: config.AdapterOpenAICompatPassthrough{
			BaseURL:   "",
			APIKey:    "",
			APIKeyEnv: "",
			Model:     "",
		},
		Pricing:     declaration.Pricing,
		WireProfile: declaration.WireProfile,
		Advertise:   false,
	}, nil
}

func selectContext(contexts []config.AdapterModelProfileContext, name string) (config.AdapterModelProfileContext, error) {
	if len(contexts) == 0 {
		return config.AdapterModelProfileContext{}, fmt.Errorf("profile has no contexts")
	}
	if strings.TrimSpace(name) == "" {
		return contexts[0], nil
	}
	for _, contextProfile := range contexts {
		if contextProfile.Name == name {
			return contextProfile, nil
		}
	}
	return config.AdapterModelProfileContext{}, fmt.Errorf("context %q is not declared by the profile", name)
}

func selectThinking(
	profiles map[string]config.AdapterModelThinkingProfile,
	name string,
) (string, int, []string, error) {
	modes := make([]string, 0, len(profiles))
	for _, profileName := range slices.Sorted(maps.Keys(profiles)) {
		mode := string(profiles[profileName].Mode)
		if !slices.Contains(modes, mode) {
			modes = append(modes, mode)
		}
	}
	if strings.TrimSpace(name) == "" {
		return "", 0, modes, nil
	}
	profile, ok := profiles[name]
	if !ok {
		return "", 0, nil, fmt.Errorf("thinking profile %q is not declared by the profile", name)
	}
	return string(profile.Mode), profile.BudgetTokens, modes, nil
}

func (registry *Registry) addExact(
	alias string,
	resolved ResolvedAlias,
	providerIsEnabled bool,
	boundEffort config.AdapterReasoningEffort,
	advertise bool,
) error {
	id := strings.TrimSpace(alias)
	if id == "" {
		return fmt.Errorf("adapter: model alias must not be empty")
	}
	key := modelKey(id)
	if _, exists := registry.exact[key]; exists {
		return fmt.Errorf("adapter: duplicate exact model alias %q", id)
	}
	if resolved.PassthroughOverride != "" {
		passthrough, ok := registry.passthroughOverrides[modelKey(resolved.PassthroughOverride)]
		if !ok {
			return fmt.Errorf("adapter: passthrough override %q is not configured", resolved.PassthroughOverride)
		}
		resolved.PassthroughConfig = passthrough
	}
	resolved.Alias = id
	resolved.Advertise = advertise && providerIsEnabled
	registry.exact[key] = registryRecord{
		resolved:        resolved,
		providerEnabled: providerIsEnabled,
		boundEffort:     boundEffort,
	}
	if resolved.Advertise {
		registry.advertised[key] = resolved
	}
	return nil
}

type aliasBindings struct {
	parts    []string
	context  string
	effort   config.AdapterReasoningEffort
	thinking string
}

func (registry *Registry) addGeneratedAliases(
	declaration config.AdapterModelDeclaration,
	profile config.AdapterModelProfile,
	providerIsEnabled bool,
) error {
	generated := declaration.GeneratedAliases
	if len(generated.Dimensions) == 0 {
		return nil
	}
	bindings := []aliasBindings{{parts: nil, context: "", effort: "", thinking: ""}}
	for _, dimension := range generated.Dimensions {
		bindings = expandAliasDimension(bindings, dimension, profile)
	}
	for _, binding := range bindings {
		alias := strings.TrimSpace(generated.Prefix)
		if len(binding.parts) > 0 {
			alias += "-" + strings.Join(binding.parts, "-")
		}
		resolved, err := resolvedFromDeclaration(alias, declaration, profile, config.AdapterModelAlias{
			ID:              alias,
			Advertise:       false,
			ReasoningEffort: binding.effort,
			Context:         binding.context,
			ThinkingProfile: binding.thinking,
		})
		if err != nil {
			return err
		}
		if err := registry.addExact(alias, resolved, providerIsEnabled, binding.effort, generated.Advertise); err != nil {
			return err
		}
	}
	return nil
}

func expandAliasDimension(
	bindings []aliasBindings,
	dimension config.AdapterGeneratedAliasDimension,
	profile config.AdapterModelProfile,
) []aliasBindings {
	switch dimension {
	case config.AdapterGeneratedAliasDimensionContext:
		out := make([]aliasBindings, 0, len(bindings)*len(profile.Contexts))
		for _, binding := range bindings {
			for _, contextProfile := range profile.Contexts {
				next := cloneBindings(binding)
				next.context = contextProfile.Name
				part := contextProfile.AliasSuffix
				if part == "" {
					part = contextProfile.Name
				}
				next.parts = append(next.parts, part)
				out = append(out, next)
			}
		}
		return out
	case config.AdapterGeneratedAliasDimensionReasoningEffort:
		out := make([]aliasBindings, 0, len(bindings)*len(profile.ReasoningEfforts))
		for _, binding := range bindings {
			for _, effort := range profile.ReasoningEfforts {
				next := cloneBindings(binding)
				next.effort = effort
				next.parts = append(next.parts, string(effort))
				out = append(out, next)
			}
		}
		return out
	case config.AdapterGeneratedAliasDimensionThinkingProfile:
		names := slices.Sorted(maps.Keys(profile.ThinkingProfiles))
		out := make([]aliasBindings, 0, len(bindings)*len(names))
		for _, binding := range bindings {
			for _, name := range names {
				next := cloneBindings(binding)
				next.thinking = name
				next.parts = append(next.parts, name)
				out = append(out, next)
			}
		}
		return out
	default:
		return nil
	}
}

func cloneBindings(binding aliasBindings) aliasBindings {
	binding.parts = append([]string(nil), binding.parts...)
	return binding
}

// Resolve applies exact, ordered route, global fallback, and omitted-default
// precedence for one typed ingress surface.
func (registry *Registry) Resolve(
	surface IngressSurface,
	requestedModel string,
	requestedEffort string,
) (ResolvedAlias, string, error) {
	if registry == nil {
		return ResolvedAlias{}, "", fmt.Errorf("adapter: model registry is nil")
	}
	requested := strings.TrimSpace(requestedModel)
	if requested == "" {
		if registry.defaultModel == "" {
			return ResolvedAlias{}, "", fmt.Errorf("model is required and adapter.default_model is not set")
		}
		requested = registry.defaultModel
		record, ok := registry.exact[modelKey(requested)]
		if !ok {
			return ResolvedAlias{}, "", fmt.Errorf("adapter.default_model %q is unavailable", requested)
		}
		return resolveExact(record, requested, requestedEffort)
	}
	if record, ok := registry.exact[modelKey(requested)]; ok {
		return resolveExact(record, requested, requestedEffort)
	}
	for _, route := range registry.routes {
		matches, err := path.Match(route.pattern, requested)
		if err != nil {
			wrapped := fmt.Errorf("match model route %q: %w", route.pattern, err)
			modelResolveLog.Logger().Warn(
				"adapter.registry.route_match_failed",
				"pattern", route.pattern,
				"model", requested,
				"err", wrapped,
			)
			return ResolvedAlias{}, "", wrapped
		}
		if !matches || !slices.Contains(route.surfaces, surface) {
			continue
		}
		if !route.providerEnabled {
			return ResolvedAlias{}, "", fmt.Errorf("model %q selects disabled provider %q", requested, route.provider)
		}
		return wildcardResolved(requested, route.provider, route.wireProfile, requestedEffort), requestedEffort, nil
	}
	if registry.openAICompat.BaseURL != "" {
		var resolved ResolvedAlias
		resolved.Alias = requested
		resolved.Backend = BackendPassthroughOverride
		resolved.WireModel = requested
		resolved.Effort = requestedEffort
		resolved.WireEffort = requestedEffort
		resolved.OpenAICompatPassthrough = registry.openAICompat
		return resolved, requestedEffort, nil
	}
	return ResolvedAlias{}, "", newResolveError(
		ResolveErrorModelNotFound,
		fmt.Sprintf("unknown model %q", requested),
	)
}

func resolveExact(record registryRecord, requested string, requestedEffort string) (ResolvedAlias, string, error) {
	if !record.providerEnabled {
		return ResolvedAlias{}, "", fmt.Errorf("model %q selects disabled provider %q", requested, record.resolved.Backend)
	}
	effort := requestedEffort
	bound := string(record.boundEffort)
	if bound != "" && effort != "" && effort != bound {
		return ResolvedAlias{}, "", newResolveError(
			ResolveErrorInvalidRequest,
			fmt.Sprintf("effort %q conflicts with effort-bound model %q", effort, requested),
		)
	}
	if effort == "" {
		effort = bound
	}
	if effort == "" {
		effort = defaultEffort(record.resolved)
	}
	if effort != "" && !slices.Contains(record.resolved.Efforts, effort) {
		return ResolvedAlias{}, "", newResolveError(
			ResolveErrorInvalidRequest,
			fmt.Sprintf(
				"effort %q not supported for %q (allowed: %s)",
				effort,
				requested,
				strings.Join(record.resolved.Efforts, ", "),
			),
		)
	}
	resolved := record.resolved
	resolved.Alias = requested
	resolved.Effort = effort
	resolved.WireEffort = effort
	if wireValue, ok := resolved.EffortWireValues[effort]; ok {
		resolved.WireEffort = wireValue
	}
	return resolved, effort, nil
}

func defaultEffort(resolved ResolvedAlias) string {
	if resolved.Effort != "" {
		return resolved.Effort
	}
	return resolved.DefaultEffort
}

func wildcardResolved(requested string, provider BackendID, wireProfile string, effort string) ResolvedAlias {
	var resolved ResolvedAlias
	resolved.Alias = requested
	resolved.Backend = provider
	resolved.WireModel = requested
	resolved.WireProfile = strings.TrimSpace(wireProfile)
	resolved.Effort = effort
	resolved.WireEffort = effort
	return resolved
}

// PassthroughOverride returns a configured named OpenAI-compatible upstream.
func (registry *Registry) PassthroughOverride(name string) (config.AdapterPassthroughOverride, bool) {
	passthrough, ok := registry.passthroughOverrides[modelKey(name)]
	return passthrough, ok
}

// List returns only explicitly advertised exact catalog entries.
func (registry *Registry) List() []ResolvedAlias {
	if registry == nil {
		return nil
	}
	aliases := slices.Sorted(maps.Keys(registry.advertised))
	out := make([]ResolvedAlias, 0, len(aliases))
	for _, alias := range aliases {
		out = append(out, registry.advertised[alias])
	}
	return out
}

// Models returns a copy of every exact catalog entry keyed by normalized ID.
func (registry *Registry) Models() map[string]ResolvedAlias {
	if registry == nil {
		return nil
	}
	out := make(map[string]ResolvedAlias, len(registry.exact))
	for key, record := range registry.exact {
		out[key] = record.resolved
	}
	return out
}

func validateAdapterCoreConfig(cfg config.AdapterConfig) error {
	if cfg.ClientIdentity.SystemPromptPrefix == "" {
		return fmt.Errorf("adapter: [adapter.client_identity].system_prompt_prefix must be set")
	}
	if cfg.ClientIdentity.StainlessPackageVersion == "" {
		return fmt.Errorf("adapter: [adapter.client_identity].stainless_package_version must be set")
	}
	if cfg.ClientIdentity.StainlessRuntime == "" {
		return fmt.Errorf("adapter: [adapter.client_identity].stainless_runtime must be set")
	}
	if cfg.ClientIdentity.StainlessRuntimeVersion == "" {
		return fmt.Errorf("adapter: [adapter.client_identity].stainless_runtime_version must be set")
	}
	if cfg.ClientIdentity.CCVersion == "" {
		return fmt.Errorf("adapter: [adapter.client_identity].cc_version must be set")
	}
	if cfg.ClientIdentity.CCEntrypoint == "" {
		return fmt.Errorf("adapter: [adapter.client_identity].cc_entrypoint must be set")
	}
	if cfg.Anthropic.Enabled || cfg.DirectOAuth {
		if err := cfg.Anthropic.OAuth.ValidateOAuthFields(); err != nil {
			slog.Warn("adapter.model.validate_anthropic_oauth_failed", "concern", "adapter.models.resolve", "err", err)
			return fmt.Errorf("validate anthropic OAuth config: %w", err)
		}
	}
	return nil
}

func validateAdapterLogprobs(logprobs config.AdapterLogprobs) error {
	if logprobs.Anthropic == "" {
		return nil
	}
	if logprobs.Anthropic != "reject" && logprobs.Anthropic != "drop" {
		return fmt.Errorf("adapter: [adapter.logprobs].anthropic %q invalid", logprobs.Anthropic)
	}
	return nil
}

func backendForProvider(provider config.AdapterModelProvider) BackendID {
	switch provider {
	case config.AdapterModelProviderCodex:
		return BackendCodex
	case config.AdapterModelProviderAnthropic:
		return BackendAnthropic
	case config.AdapterModelProviderPassthroughOverride:
		return BackendPassthroughOverride
	default:
		return BackendID(provider)
	}
}

func providerEnabled(cfg config.AdapterConfig, provider config.AdapterModelProvider) bool {
	switch provider {
	case config.AdapterModelProviderCodex:
		return cfg.Codex.Enabled
	case config.AdapterModelProviderAnthropic:
		return cfg.Anthropic.Enabled || cfg.DirectOAuth
	case config.AdapterModelProviderPassthroughOverride:
		return true
	default:
		return false
	}
}

func defaultWireProfile(cfg config.AdapterConfig, provider config.AdapterModelProvider) string {
	if provider != config.AdapterModelProviderAnthropic {
		return ""
	}
	return strings.TrimSpace(cfg.Anthropic.DefaultWireProfile)
}

func modelKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// ClaudeEffortFlag translates legacy Claude subprocess effort values.
func ClaudeEffortFlag(tier string) string {
	switch strings.ToLower(tier) {
	case EffortLow:
		return EffortLow
	case EffortMedium, "med":
		return EffortMedium
	case EffortHigh:
		return EffortHigh
	case EffortMax:
		return EffortMax
	default:
		return ""
	}
}
