package model

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/config"
)

// Backend names the kind of worker that fulfils a request.
const (
	BackendClaude              = "claude"
	BackendPassthroughOverride = "passthrough_override"
	// BackendAnthropic routes the request directly at the configured
	// messages URL. Auth uses the token from the keychain (the
	// internal/adapter/oauth package handles credentials;
	// internal/adapter/anthropic handles HTTP). Selected
	// when adapter config sets direct_oauth=true; the registry
	// rewrites every BackendClaude model to BackendAnthropic at
	// construction so the HTTP layer only has to switch on this
	// single value.
	BackendAnthropic = "anthropic"
	// BackendCodex routes to ChatGPT/Codex-backed model execution.
	BackendCodex = "codex"
)

// Effort tiers (low, medium, high, max). Sent inside output_config
// on the messages API when the beta header set allows it. Whether a
// given family accepts a given tier is
// declared in the user's toml under [adapter.families.<name>.efforts].
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"
)

// Thinking modes for the extended-thinking wire shape. The adapter
// translates each value into the right payload on the direct HTTP
// path. `default` means "send no thinking block"
// so the server applies its per-model default.
const (
	ThinkingDefault  = "default"
	ThinkingAdaptive = "adaptive"
	ThinkingEnabled  = "enabled"
	ThinkingDisabled = "disabled"
)

// ResolvedModel is the runtime view of one alias. The registry maps
// every incoming model string to exactly one ResolvedModel.
type ResolvedModel struct {
	// Alias is the public name the client sent.
	Alias string
	// Backend is one of BackendClaude / BackendPassthroughOverride /
	// BackendAnthropic / BackendCodex.
	Backend string
	// ClaudeModel names the real Claude snapshot. May carry a
	// context-window wire suffix (e.g. "[1m]");
	// oauth_handler.stripContextSuffix removes it before the wire
	// call.
	ClaudeModel string
	// Instructions carries model-specific developer instructions loaded
	// from config. The registry preserves the file contents verbatim.
	Instructions string
	// Context is the advertised context window in tokens. Purely
	// informational; the upstream enforces the real limit.
	Context int
	// ObservedContext is the provider-specific context window Clyde
	// should expose for capability reports when it differs from the
	// advertised context. Zero means use Context.
	ObservedContext int
	// Efforts enumerates the allowed `effort` values for this
	// family. Empty for families the toml leaves without an efforts
	// list; Resolve will then 400 any caller-supplied effort.
	Efforts []string
	// Effort is the bound effort value when the alias encodes one
	// (e.g. clyde-opus-4.7-1m-high -> "high"). Empty when the
	// caller picks per-request via the OpenAI body.
	Effort string
	// ThinkingModes enumerates the allowed `thinking` values.
	ThinkingModes []string
	// Thinking is the bound thinking mode when the alias encodes
	// one. Empty resolves to ThinkingDefault at request time.
	Thinking string
	// MaxOutputTokens caps the family's output tokens. Used to
	// derive the budget_tokens for ThinkingEnabled (budget = max-1).
	MaxOutputTokens int
	// SupportsTools mirrors the family toml after NewRegistry
	// validation (nil disallowed at load time).
	SupportsTools bool
	// SupportsVision mirrors the family toml after NewRegistry
	// validation (nil disallowed at load time).
	SupportsVision bool
	// PassthroughOverride names an entry inside
	// AdapterConfig.PassthroughOverrides. Only set when Backend is
	// BackendPassthroughOverride and the alias uses a named passthrough
	// override.
	PassthroughOverride string
	// OpenAICompatPassthrough carries a directly configured upstream
	// endpoint for unknown-model passthrough. Only set when Backend is
	// BackendPassthroughOverride and PassthroughOverride is empty.
	OpenAICompatPassthrough config.AdapterOpenAICompatPassthrough
	// FamilySlug is the cfg.Families key this alias was generated
	// from. Empty for user-supplied [adapter.models.<name>] entries
	// (which carry their own backend/model directly).
	FamilySlug string
}

// Registry owns the resolved model table used by the HTTP layer.
// Construction layers user-supplied per-alias overrides on top of
// the family-expanded aliases. There are no compiled-in defaults:
// NewRegistry rejects an AdapterConfig that omits families,
// client_identity fields, or default_model.
type Registry struct {
	models                         map[string]ResolvedModel
	passthroughOverrides           map[string]config.AdapterPassthroughOverride
	def                            string
	openAICompat                   config.AdapterOpenAICompatPassthrough
	codexEnabled                   bool
	codexPrefix                    []string
	codexNativeRouting             string
	codexNativePassthroughOverride string
	codexModels                    map[string]ResolvedModel
	nativeCodexModels              map[string]ResolvedModel
	nativeAdvertised               map[string]bool
}

// NewRegistry builds the registry from a loaded AdapterConfig. It
// returns an error when the config is incomplete: no families,
// no default model, or any required client_identity / oauth field
// empty. Callers should refuse to start the listener and surface the
// error so the user sees what is missing.
func NewRegistry(cfg config.AdapterConfig) (*Registry, error) {
	if err := validateAdapterCoreConfig(cfg); err != nil {
		return nil, err
	}
	toolsCapable, visionCapable, err := countFamilyCapabilities(cfg.Families)
	if err != nil {
		return nil, err
	}
	if err := validateAdapterLogprobs(cfg.Logprobs); err != nil {
		return nil, err
	}
	if len(cfg.Families) == 0 {
		return nil, fmt.Errorf("adapter: no model families declared in [adapter.families]")
	}
	models, err := buildFamilyAliases(cfg.Families)
	if err != nil {
		return nil, err
	}

	r := newEmptyRegistry(cfg, models)
	if err := finalizeCodexNativeRouting(r, cfg); err != nil {
		return nil, err
	}
	if len(r.codexPrefix) == 0 {
		r.codexPrefix = []string{"gpt-", "o"}
	}

	if err := loadConfigModels(r, cfg.Models); err != nil {
		return nil, err
	}
	for name, s := range cfg.PassthroughOverrides {
		r.passthroughOverrides[strings.ToLower(name)] = s
	}
	if err := loadCodexModels(r, cfg.Codex.Models); err != nil {
		return nil, err
	}

	applyDirectOAuthRewrite(r, cfg)

	if _, ok := r.models[strings.ToLower(r.def)]; !ok {
		// The default model must resolve after expansion. Permit a
		// passthrough upstream to absorb it; otherwise hard fail because a
		// daemon that cannot serve its declared default is broken.
		if r.openAICompat.BaseURL == "" {
			return nil, fmt.Errorf(
				"adapter: default_model %q not found among %d expanded aliases",
				r.def, len(r.models),
			)
		}
	}

	modelCatalogLog.Logger().Info("adapter.registry.capabilities_loaded",
		"subcomponent", "adapter",
		"families", len(cfg.Families),
		"tools_capable", toolsCapable,
		"vision_capable", visionCapable,
	)

	return r, nil
}

// validateAdapterCoreConfig enforces the always-required adapter fields.
func validateAdapterCoreConfig(cfg config.AdapterConfig) error {
	if cfg.DefaultModel == "" {
		return fmt.Errorf("adapter: default_model must be set in [adapter]")
	}
	// beta_header and user_agent are optional. When empty, the
	// adapter falls through to the captured WireFlavor in
	// internal/adapter/anthropic/wire_flavors_gen.go (CLYDE-124).
	// Setting either is an explicit operator override.
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
	if cfg.DirectOAuth {
		if err := cfg.Anthropic.OAuth.ValidateOAuthFields(); err != nil {
			return err
		}
	}
	return nil
}

// countFamilyCapabilities validates each family's required capability flags
// and returns aggregate tool/vision counts for the load-time log line.
func countFamilyCapabilities(families map[string]config.AdapterFamily) (int, int, error) {
	toolsCapable := 0
	visionCapable := 0
	for slug, family := range families {
		if family.SupportsTools == nil {
			return 0, 0, fmt.Errorf(
				"adapter: family %q missing supports_tools (must be explicit true/false)",
				slug,
			)
		}
		if family.SupportsVision == nil {
			return 0, 0, fmt.Errorf("adapter: family %q missing supports_vision", slug)
		}
		if *family.SupportsTools {
			toolsCapable++
		}
		if *family.SupportsVision {
			visionCapable++
		}
	}
	return toolsCapable, visionCapable, nil
}

// buildFamilyAliases validates each family and expands the alias cross-product.
func buildFamilyAliases(families map[string]config.AdapterFamily) (map[string]ResolvedModel, error) {
	models := map[string]ResolvedModel{}
	for slug, family := range families {
		if family.Model == "" {
			return nil, fmt.Errorf("adapter: family %q missing model id", slug)
		}
		if len(family.Contexts) == 0 {
			return nil, fmt.Errorf("adapter: family %q missing contexts", slug)
		}
		switch family.ThinkingWireMode {
		case "", ThinkingEnabled, ThinkingAdaptive:
		default:
			return nil, fmt.Errorf("adapter: family %q has invalid thinking_wire_mode %q (allowed: %q, %q)", slug, family.ThinkingWireMode, ThinkingEnabled, ThinkingAdaptive)
		}
		// Resolve the implicit per-family fallback for thinking_wire_mode.
		// The result is what generateFamilyAliases will stamp onto every
		// thinking-enabled alias. EffectiveThinkingMode does not patch
		// this at request time; the family's choice is the wire choice.
		family = withResolvedThinkingWireMode(slog.Default(), slug, family)
		generateFamilyAliases(models, slug, family)
	}
	return models, nil
}

// newEmptyRegistry constructs a Registry seeded with the family-expanded
// alias map and the codex-related config knobs in their pre-validated form.
func newEmptyRegistry(cfg config.AdapterConfig, models map[string]ResolvedModel) *Registry {
	return &Registry{
		models:                         models,
		passthroughOverrides:           map[string]config.AdapterPassthroughOverride{},
		def:                            cfg.DefaultModel,
		openAICompat:                   cfg.OpenAICompatPassthrough,
		codexEnabled:                   cfg.Codex.Enabled,
		codexPrefix:                    append([]string(nil), cfg.Codex.ModelPrefixes...),
		codexNativeRouting:             strings.ToLower(strings.TrimSpace(cfg.Codex.NativeModelRouting)),
		codexNativePassthroughOverride: strings.ToLower(strings.TrimSpace(cfg.Codex.NativeModelPassthroughOverride)),
		codexModels:                    map[string]ResolvedModel{},
		nativeCodexModels:              map[string]ResolvedModel{},
		nativeAdvertised:               map[string]bool{},
	}
}

// finalizeCodexNativeRouting validates codex native routing settings and
// applies the implicit default when the operator left the field blank.
func finalizeCodexNativeRouting(r *Registry, cfg config.AdapterConfig) error {
	if r.codexNativeRouting == "" {
		if r.codexEnabled {
			r.codexNativeRouting = "codex"
		} else {
			r.codexNativeRouting = "off"
		}
	}
	switch r.codexNativeRouting {
	case "off", "codex", BackendPassthroughOverride:
	default:
		return fmt.Errorf("adapter: [adapter.codex].native_model_routing must be one of: off, codex, passthrough_override")
	}
	if r.codexNativeRouting == BackendPassthroughOverride {
		if r.codexNativePassthroughOverride == "" {
			return fmt.Errorf("adapter: [adapter.codex].native_model_passthrough_override is required when native_model_routing = \"passthrough_override\"")
		}
		if _, ok := cfg.PassthroughOverrides[r.codexNativePassthroughOverride]; !ok {
			return fmt.Errorf("adapter: [adapter.codex].native_model_passthrough_override %q not found in [adapter.passthrough_overrides]", r.codexNativePassthroughOverride)
		}
	}
	return nil
}

// loadConfigModels copies user-supplied [adapter.models.*] entries into the
// registry, rejecting the legacy "fallback" backend.
func loadConfigModels(r *Registry, models map[string]config.AdapterModel) error {
	for name, m := range models {
		if strings.EqualFold(strings.TrimSpace(m.Backend), "fallback") {
			return fmt.Errorf("adapter: [adapter.models.%s].backend = %q is no longer supported", name, m.Backend)
		}
		r.models[strings.ToLower(name)] = resolveFromConfig(name, m)
	}
	return nil
}

// loadCodexModels expands every [adapter.codex.models] entry into both the
// effort-keyed alias table and the native-alias table.
func loadCodexModels(r *Registry, models []config.AdapterCodexModel) error {
	for _, model := range models {
		if err := addCodexModelAliases(r.codexModels, model); err != nil {
			return err
		}
		if err := addNativeCodexModelAliases(r.nativeCodexModels, r.nativeAdvertised, model); err != nil {
			return err
		}
	}
	return nil
}

// applyDirectOAuthRewrite stamps the alias onto every resolved model, and
// when DirectOAuth is on rewrites BackendClaude entries to BackendAnthropic.
func applyDirectOAuthRewrite(r *Registry, cfg config.AdapterConfig) {
	rewritten := 0
	for alias, rm := range r.models {
		rm.Alias = alias
		if cfg.DirectOAuth && rm.Backend == BackendClaude {
			rm.Backend = BackendAnthropic
			rewritten++
		}
		r.models[alias] = rm
	}
	if cfg.DirectOAuth {
		modelCatalogLog.Logger().Info("adapter.registry.oauth_rewrite",
			"subcomponent", "adapter",
			"models_rewritten", rewritten,
			"models_total", len(r.models),
		)
	}
}

// validateAdapterLogprobs enforces the live Anthropic logprobs policy.
func validateAdapterLogprobs(lp config.AdapterLogprobs) error {
	if lp.Anthropic == "" {
		return nil
	}
	allowed := map[string]bool{"reject": true, "drop": true}
	if !allowed[lp.Anthropic] {
		return fmt.Errorf("adapter: [adapter.logprobs].anthropic %q invalid", lp.Anthropic)
	}
	return nil
}

// withResolvedThinkingWireMode populates family.ThinkingWireMode when
// it is empty by applying any known per-model fallback. Today the only
// fallback is the historical claude-opus-4-7 rule: that family's
// upstream rejects enabled-mode thinking and requires adaptive, so an
// unset config gets remapped to adaptive at registry construction. The
// remap fires a one-shot Warn so operators see the implicit mapping
// and can set thinking_wire_mode explicitly in their config to silence
// it. Operators who explicitly set ThinkingWireMode (including
// "enabled") are honored; this fallback never overrides them.
func withResolvedThinkingWireMode(log *slog.Logger, slug string, f config.AdapterFamily) config.AdapterFamily {
	if f.ThinkingWireMode != "" {
		return f
	}
	if !contains(f.ThinkingModes, ThinkingEnabled) {
		return f
	}
	if !strings.EqualFold(f.Model, "claude-opus-4-7") {
		return f
	}
	if log != nil {
		log.Warn("adapter.family.thinking_wire_mode_implicit",
			"component", "adapter",
			"subcomponent", "models",
			"family", slug,
			"model", f.Model,
			"effective_mode", ThinkingAdaptive,
			"reason", "claude-opus-4-7 historically rejected enabled-mode thinking. The registry is mapping enabled to adaptive at registry construction. Set thinking_wire_mode = \"adaptive\" in [adapter.families."+slug+"] to make this explicit and silence this warning.",
		)
	}
	f.ThinkingWireMode = ThinkingAdaptive
	return f
}

// generateFamilyAliases produces the full cross product of effort ×
// context × thinking-enabled aliases for one family declaration. Schema:
//
//	clyde-<family>[-<ctx>]-<effort>[-thinking]
//
// Absence of -thinking is the canonical disabled-thinking variant.
func generateFamilyAliases(out map[string]ResolvedModel, slug string, f config.AdapterFamily) {
	if len(f.Efforts) == 0 {
		return
	}
	family := strings.TrimSpace(f.AliasPrefix)
	if family == "" {
		family = slug
	}
	emitThinking := contains(f.ThinkingModes, ThinkingEnabled)
	thinkingWire := f.ThinkingWireMode
	if thinkingWire == "" {
		thinkingWire = ThinkingEnabled
	}
	for _, ctx := range f.Contexts {
		for _, eff := range f.Efforts {
			base := ResolvedModel{
				Alias:                   "",
				Backend:                 BackendClaude,
				ClaudeModel:             f.Model + ctx.WireSuffix,
				Instructions:            f.Instructions,
				Context:                 ctx.Tokens,
				ObservedContext:         ctx.ObservedTokens,
				Efforts:                 []string{eff},
				Effort:                  eff,
				ThinkingModes:           f.ThinkingModes,
				Thinking:                ThinkingDisabled,
				MaxOutputTokens:         f.MaxOutputTokens,
				SupportsTools:           *f.SupportsTools,
				SupportsVision:          *f.SupportsVision,
				PassthroughOverride:     "",
				OpenAICompatPassthrough: config.AdapterOpenAICompatPassthrough{BaseURL: "", APIKey: "", APIKeyEnv: "", Model: ""},
				FamilySlug:              slug,
			}
			out[buildAlias(family, ctx.AliasSuffix, eff, false)] = base
			if emitThinking {
				thinkingModel := base
				thinkingModel.Thinking = thinkingWire
				out[buildAlias(family, ctx.AliasSuffix, eff, true)] = thinkingModel
			}
		}
		out[buildAlias(family, ctx.AliasSuffix, "", false)] = ResolvedModel{
			Alias:                   "",
			Backend:                 BackendClaude,
			ClaudeModel:             f.Model + ctx.WireSuffix,
			Instructions:            f.Instructions,
			Context:                 ctx.Tokens,
			ObservedContext:         ctx.ObservedTokens,
			Efforts:                 f.Efforts,
			Effort:                  "",
			ThinkingModes:           f.ThinkingModes,
			Thinking:                ThinkingDisabled,
			MaxOutputTokens:         f.MaxOutputTokens,
			SupportsTools:           *f.SupportsTools,
			SupportsVision:          *f.SupportsVision,
			PassthroughOverride:     "",
			OpenAICompatPassthrough: config.AdapterOpenAICompatPassthrough{BaseURL: "", APIKey: "", APIKeyEnv: "", Model: ""},
			FamilySlug:              slug,
		}
	}
}

// buildAlias assembles one alias from its components.
func buildAlias(family, ctxSuffix, effort string, thinking bool) string {
	parts := []string{"clyde", family}
	if ctxSuffix != "" {
		parts = append(parts, ctxSuffix)
	}
	if effort != "" {
		parts = append(parts, effort)
	}
	if thinking {
		parts = append(parts, "thinking")
	}
	return strings.Join(parts, "-")
}

func addCodexModelAliases(out map[string]ResolvedModel, cfg config.AdapterCodexModel) error {
	if strings.TrimSpace(cfg.AliasPrefix) == "" {
		return fmt.Errorf("adapter: [adapter.codex.models] entry missing alias_prefix")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("adapter: [adapter.codex.models.%s] missing model", cfg.AliasPrefix)
	}
	if len(cfg.Efforts) == 0 {
		return fmt.Errorf("adapter: [adapter.codex.models.%s] missing efforts", cfg.AliasPrefix)
	}
	if len(cfg.Contexts) == 0 {
		return fmt.Errorf("adapter: [adapter.codex.models.%s] missing contexts", cfg.AliasPrefix)
	}
	for _, ctx := range cfg.Contexts {
		for _, effort := range cfg.Efforts {
			alias := buildAlias(cfg.AliasPrefix, ctx.AliasSuffix, effort, false)
			out[strings.ToLower(alias)] = ResolvedModel{
				Alias:           alias,
				Backend:         BackendCodex,
				ClaudeModel:     cfg.Model,
				Instructions:    cfg.Instructions,
				Context:         ctx.Tokens,
				ObservedContext: ctx.ObservedTokens,
				Efforts:         []string{effort},
				Effort:          effort,
				MaxOutputTokens: cfg.MaxOutputTokens,
			}
		}
	}
	for _, ctx := range cfg.Contexts {
		alias := buildAlias(cfg.AliasPrefix, ctx.AliasSuffix, "", false)
		out[strings.ToLower(alias)] = ResolvedModel{
			Alias:           alias,
			Backend:         BackendCodex,
			ClaudeModel:     cfg.Model,
			Instructions:    cfg.Instructions,
			Context:         ctx.Tokens,
			ObservedContext: ctx.ObservedTokens,
			Efforts:         cfg.Efforts,
			MaxOutputTokens: cfg.MaxOutputTokens,
		}
	}
	return nil
}

func addNativeCodexModelAliases(out map[string]ResolvedModel, advertised map[string]bool, cfg config.AdapterCodexModel) error {
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("adapter: [adapter.codex.models.%s] missing model", cfg.AliasPrefix)
	}
	if len(cfg.Contexts) == 0 {
		return fmt.Errorf("adapter: [adapter.codex.models.%s] missing contexts", cfg.AliasPrefix)
	}
	for _, ctx := range cfg.Contexts {
		nativeAliases := append([]string(nil), ctx.NativeAliases...)
		nativeAliases = append(nativeAliases, ctx.AdvertisedNativeAliases...)
		for _, alias := range nativeAliases {
			addNativeCodexModelAlias(out, alias, cfg, ctx)
		}
		for _, alias := range ctx.AdvertisedNativeAliases {
			alias = strings.TrimSpace(alias)
			if alias != "" {
				advertised[strings.ToLower(alias)] = true
			}
		}
	}
	return nil
}

func addNativeCodexModelAlias(out map[string]ResolvedModel, alias string, cfg config.AdapterCodexModel, ctx config.AdapterCodexModelContext) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return
	}
	key := strings.ToLower(alias)
	if _, ok := out[key]; ok {
		return
	}
	out[key] = ResolvedModel{
		Alias:           alias,
		Backend:         BackendCodex,
		ClaudeModel:     cfg.Model,
		Instructions:    cfg.Instructions,
		Context:         ctx.Tokens,
		ObservedContext: ctx.ObservedTokens,
		Efforts:         cfg.Efforts,
		MaxOutputTokens: cfg.MaxOutputTokens,
	}
}

// resolveFromConfig converts a user provided AdapterModel entry into
// a ResolvedModel. Missing fields default to the claude backend so
// a minimal config stanza (just a Model name) works.
func resolveFromConfig(alias string, m config.AdapterModel) ResolvedModel {
	backend := m.Backend
	if backend == "" {
		if m.PassthroughOverride != "" {
			backend = BackendPassthroughOverride
		} else {
			backend = BackendClaude
		}
	}
	out := ResolvedModel{
		Alias:               alias,
		Backend:             backend,
		ClaudeModel:         m.Model,
		Instructions:        m.Instructions,
		Context:             m.Context,
		ObservedContext:     m.ObservedContext,
		Efforts:             m.Efforts,
		PassthroughOverride: m.PassthroughOverride,
	}
	return out
}

func ResolveFromConfig(alias string, m config.AdapterModel) ResolvedModel {
	return resolveFromConfig(alias, m)
}

func (r *Registry) looksLikeCodexModel(alias string) bool {
	if !r.codexEnabled {
		return false
	}
	key := strings.ToLower(strings.TrimSpace(alias))
	if key == "" {
		return false
	}
	if strings.HasPrefix(key, "clyde-o1") ||
		strings.HasPrefix(key, "clyde-o3") ||
		strings.HasPrefix(key, "clyde-o4") ||
		strings.HasPrefix(key, "clyde-codex-") {
		return true
	}
	return r.looksLikeNativeCodexModel(alias)
}

func (r *Registry) looksLikeNativeCodexModel(alias string) bool {
	if !r.codexEnabled {
		return false
	}
	key := strings.ToLower(strings.TrimSpace(alias))
	if key == "" || strings.HasPrefix(key, "clyde-") {
		return false
	}
	for _, p := range r.codexPrefix {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func normalizeCodexModelAlias(alias string) string {
	key := codexAliasModelKey(alias)
	lower := strings.ToLower(key)
	for _, prefix := range []string{"clyde-codex-"} {
		if strings.HasPrefix(lower, prefix) {
			key = key[len(prefix):]
			lower = strings.ToLower(key)
			break
		}
	}
	if strings.HasPrefix(lower, "clyde-gpt-") {
		key = "gpt-" + key[len("clyde-gpt-"):]
	}
	return key
}

func codexAliasModelKey(alias string) string {
	key := strings.TrimSpace(alias)
	lower := strings.ToLower(key)
	for _, suffix := range []string{"-low", "-medium", "-high", "-xhigh"} {
		if strings.HasSuffix(lower, suffix) {
			key = key[:len(key)-len(suffix)]
			break
		}
	}
	return key
}

func codexAliasEffort(alias string) string {
	lower := strings.ToLower(strings.TrimSpace(alias))
	for _, suffix := range []string{"xhigh", "high", "medium", "low"} {
		if strings.HasSuffix(lower, "-"+suffix) {
			return suffix
		}
	}
	return ""
}

// Resolve looks up the alias and returns the resolved model plus the
// chosen effort tier. reqEffort may be empty; the registry uses the
// alias-bound effort first, then the family default. Unknown aliases
// fall through to the registry default alias (or the configured
// OpenAI-compatible passthrough upstream).
//
// Returns an error when the caller asks for an effort value the
// alias's family doesn't support server-side. This surfaces as a
// 400 to the OpenAI client rather than letting the upstream return
// a less actionable message.
func (r *Registry) Resolve(alias, reqEffort string) (ResolvedModel, string, error) {
	if alias == "" {
		alias = r.def
	}
	key := strings.ToLower(alias)
	if m, ok := r.models[key]; ok {
		return resolveConfiguredModel(m, reqEffort)
	}
	if r.looksLikeNativeCodexModel(alias) {
		switch r.codexNativeRouting {
		case "codex":
			return r.resolveNativeCodexModel(alias, reqEffort)
		case BackendPassthroughOverride:
			if _, ok := r.passthroughOverrides[r.codexNativePassthroughOverride]; ok {
				return ResolvedModel{
					Alias:               alias,
					Backend:             BackendPassthroughOverride,
					PassthroughOverride: r.codexNativePassthroughOverride,
				}, "", nil
			}
			return ResolvedModel{}, "", fmt.Errorf("native model passthrough override %q is not configured", r.codexNativePassthroughOverride)
		default:
			return ResolvedModel{}, "", fmt.Errorf(
				"unknown model %q (native model routing is off; configure [adapter.models.%q] or [adapter.codex].native_model_routing)",
				alias,
				alias,
			)
		}
	}
	if m, ok := r.codexModels[key]; ok {
		effort := strings.ToLower(strings.TrimSpace(reqEffort))
		switch {
		case effort == "":
			if m.Effort == "" {
				return ResolvedModel{}, "", fmt.Errorf(
					"model %q requires an explicit effort-qualified alias",
					m.Alias,
				)
			}
			effort = m.Effort
		case m.Effort != "" && effort != m.Effort:
			return ResolvedModel{}, "", fmt.Errorf(
				"effort %q conflicts with effort-bound model %q",
				effort, m.Alias,
			)
		case m.Effort == "" && !contains(m.Efforts, effort):
			return ResolvedModel{}, "", fmt.Errorf(
				"effort %q not supported for %q (allowed: %s)",
				effort, m.Alias, strings.Join(m.Efforts, ", "),
			)
		}
		return m, effort, nil
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(alias)), "clyde-gpt-") {
		return ResolvedModel{}, "", fmt.Errorf(
			"unknown clyde GPT model %q (declare it under [adapter.codex.models] with an explicit effort alias)",
			alias,
		)
	}
	if r.looksLikeCodexModel(alias) {
		return r.resolveNativeCodexModel(alias, reqEffort)
	}
	if r.openAICompat.BaseURL != "" {
		return ResolvedModel{
			Alias:                   alias,
			Backend:                 BackendPassthroughOverride,
			OpenAICompatPassthrough: r.openAICompat,
		}, "", nil
	}
	def, dok := r.models[strings.ToLower(r.def)]
	if !dok {
		return ResolvedModel{}, "", fmt.Errorf("unknown model %q and no usable default", alias)
	}
	return resolveConfiguredModel(def, reqEffort)
}

func (r *Registry) resolveNativeCodexModel(alias, reqEffort string) (ResolvedModel, string, error) {
	effort := strings.ToLower(strings.TrimSpace(reqEffort))
	if effort == "" {
		effort = codexAliasEffort(alias)
	}
	modelKey := strings.ToLower(strings.TrimSpace(normalizeCodexModelAlias(alias)))
	if m, ok := r.nativeCodexModels[modelKey]; ok {
		m.Alias = alias
		if effort != "" && len(m.Efforts) > 0 && !contains(m.Efforts, effort) {
			return ResolvedModel{}, "", fmt.Errorf(
				"effort %q not supported for %q (allowed: %s)",
				effort, alias, strings.Join(m.Efforts, ", "),
			)
		}
		return m, effort, nil
	}
	return ResolvedModel{
		Alias:       alias,
		Backend:     BackendCodex,
		ClaudeModel: normalizeCodexModelAlias(alias),
	}, effort, nil
}

func resolveConfiguredModel(m ResolvedModel, reqEffort string) (ResolvedModel, string, error) {
	effort := strings.ToLower(reqEffort)
	if effort == "" {
		if m.Effort != "" {
			effort = m.Effort
		} else if len(m.Efforts) > 0 {
			effort = m.Efforts[0]
		}
	} else {
		if len(m.Efforts) == 0 {
			return ResolvedModel{}, "", fmt.Errorf(
				"model %q does not accept effort (family declares no efforts in toml)",
				m.Alias,
			)
		}
		if !contains(m.Efforts, effort) {
			return ResolvedModel{}, "", fmt.Errorf(
				"effort %q not supported for %q (allowed: %s)",
				effort, m.Alias, strings.Join(m.Efforts, ", "),
			)
		}
	}
	return m, effort, nil
}

// PassthroughOverride returns the passthrough override config for a named entry.
func (r *Registry) PassthroughOverride(name string) (config.AdapterPassthroughOverride, bool) {
	s, ok := r.passthroughOverrides[strings.ToLower(name)]
	return s, ok
}

// List returns the resolved models for /v1/models. Order is
// undefined; callers that care should sort by Alias.
func (r *Registry) List() []ResolvedModel {
	out := make([]ResolvedModel, 0, len(r.models)+len(r.codexModels))
	seen := make(map[string]struct{}, len(r.models)+len(r.codexModels))
	for _, m := range r.models {
		out = append(out, m)
		seen[strings.ToLower(strings.TrimSpace(m.Alias))] = struct{}{}
	}
	for _, m := range r.codexModels {
		out = append(out, m)
		seen[strings.ToLower(strings.TrimSpace(m.Alias))] = struct{}{}
	}
	if r.codexEnabled && r.codexNativeRouting == "codex" {
		for _, alias := range slices.Sorted(maps.Keys(r.nativeAdvertised)) {
			if _, ok := seen[alias]; ok {
				continue
			}
			out = append(out, r.nativeCodexModels[alias])
		}
	}
	return out
}

func (r *Registry) Models() map[string]ResolvedModel {
	out := make(map[string]ResolvedModel, len(r.models))
	maps.Copy(out, r.models)
	return out
}

func contains(s []string, v string) bool {
	return slices.Contains(s, v)
}

// ClaudeEffortFlag translates an effort tier into the string passed
// to claude -p --effort on the legacy backend. Empty input (or any
// unrecognised tier) returns empty so the caller can omit the flag.
func ClaudeEffortFlag(tier string) string {
	switch strings.ToLower(tier) {
	case EffortLow:
		return "low"
	case EffortMedium, "med":
		return "medium"
	case EffortHigh:
		return "high"
	case EffortMax:
		return "max"
	}
	return ""
}
