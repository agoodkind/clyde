package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/util"
)

// loadConfig tries to load config.toml from a directory.
// Uses pelletier/go-toml/v2; the older BurntSushi/toml dep is now unmaintained
// and was removed. Pelletier mirrors the same Marshal / Unmarshal API surface
// so the migration is a one-line import swap on each call.
func loadConfig(dir string) (*Config, error) {
	log := slog.Default().With("concern", "process.daemon.config")
	tomlPath := filepath.Join(dir, "config.toml")
	if util.FileExists(tomlPath) {
		var cfg Config
		data, err := os.ReadFile(tomlPath)
		if err != nil {
			log.Warn("config.load.read_failed",
				"component", "config",
				"subcomponent", "load",
				"path", tomlPath,
				"format", "toml",
				"err", err,
			)
			return nil, fmt.Errorf("failed to read %s: %w", tomlPath, err)
		}
		if err := toml.Unmarshal(data, &cfg); err != nil {
			log.Warn("config.load.parse_failed",
				"component", "config",
				"subcomponent", "load",
				"path", tomlPath,
				"format", "toml",
				"err", err,
			)
			return nil, fmt.Errorf("failed to parse %s: %w", tomlPath, err)
		}
		if err := hydrateAdapterInstructionFiles(&cfg, tomlPath); err != nil {
			log.Warn("config.load.instructions_failed",
				"component", "config",
				"subcomponent", "load",
				"path", tomlPath,
				"format", "toml",
				"err", err,
			)
			return nil, fmt.Errorf("invalid %s: %w", tomlPath, err)
		}
		if err := applyLoggingDefaultsAndValidate(&cfg); err != nil {
			log.Warn("config.load.validate_failed",
				"component", "config",
				"subcomponent", "load",
				"path", tomlPath,
				"format", "toml",
				"err", err,
			)
			return nil, fmt.Errorf("invalid %s: %w", tomlPath, err)
		}
		log.Debug("config.load.loaded",
			"component", "config",
			"subcomponent", "load",
			"format", "toml",
			"path", tomlPath,
		)
		return &cfg, nil
	}

	return nil, os.ErrNotExist
}

func hydrateAdapterInstructionFiles(cfg *Config, configPath string) error {
	if cfg == nil {
		return nil
	}
	log := slog.Default().With("concern", "process.daemon.config")
	configDir := filepath.Dir(configPath)
	for name, model := range cfg.Adapter.Models {
		contents, err := loadInstructionFile(configDir, model.InstructionsFile)
		if err != nil {
			log.Warn("config.load.instructions_file_failed",
				"component", "config",
				"subcomponent", "load",
				"scope", "adapter.models",
				"name", name,
				"path", model.InstructionsFile,
				"err", err,
			)
			return fmt.Errorf("adapter.models.%s.instructions_file: %w", name, err)
		}
		model.Instructions = contents
		cfg.Adapter.Models[name] = model
	}
	for name, family := range cfg.Adapter.Families {
		contents, err := loadInstructionFile(configDir, family.InstructionsFile)
		if err != nil {
			log.Warn("config.load.instructions_file_failed",
				"component", "config",
				"subcomponent", "load",
				"scope", "adapter.families",
				"name", name,
				"path", family.InstructionsFile,
				"err", err,
			)
			return fmt.Errorf("adapter.families.%s.instructions_file: %w", name, err)
		}
		family.Instructions = contents
		cfg.Adapter.Families[name] = family
	}
	for i, model := range cfg.Adapter.Codex.Models {
		contents, err := loadInstructionFile(configDir, model.InstructionsFile)
		if err != nil {
			aliasPrefix := strings.TrimSpace(model.AliasPrefix)
			if aliasPrefix == "" {
				aliasPrefix = fmt.Sprintf("#%d", i)
			}
			log.Warn("config.load.instructions_file_failed",
				"component", "config",
				"subcomponent", "load",
				"scope", "adapter.codex.models",
				"name", aliasPrefix,
				"path", model.InstructionsFile,
				"err", err,
			)
			return fmt.Errorf("adapter.codex.models.%s.instructions_file: %w", aliasPrefix, err)
		}
		model.Instructions = contents
		cfg.Adapter.Codex.Models[i] = model
	}
	return nil
}

func loadInstructionFile(configDir string, configuredPath string) (string, error) {
	trimmedPath := strings.TrimSpace(configuredPath)
	if trimmedPath == "" {
		return "", nil
	}
	resolvedPath := trimmedPath
	if !filepath.IsAbs(resolvedPath) {
		resolvedPath = filepath.Join(configDir, resolvedPath)
	}
	contents, err := os.ReadFile(resolvedPath)
	if err != nil {
		slog.Default().With("concern", "process.daemon.config").Warn("config.load.instructions_file_read_failed",
			"component", "config",
			"subcomponent", "load",
			"path", resolvedPath,
			"err", err,
		)
		return "", err
	}
	if len(contents) == 0 {
		err := fmt.Errorf("read %q: file is empty", resolvedPath)
		slog.Default().With("concern", "process.daemon.config").Warn("config.load.instructions_file_empty",
			"component", "config",
			"subcomponent", "load",
			"path", resolvedPath,
			"err", err,
		)
		return "", err
	}
	return string(contents), nil
}

// LoadGlobalOrDefault loads the global ~/.config/clyde/ config.
// Returns empty config if config.toml does not exist.
func LoadGlobalOrDefault() (*Config, error) {
	globalDir := filepath.Dir(GlobalConfigPath()) // ~/.config/clyde/
	cfg, err := loadConfig(globalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return NewConfigWithDefaults(), nil
		}
		return nil, err
	}
	if err := applyLoggingDefaultsAndValidate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SaveGlobal writes the config back to the global location as TOML.
// The directory is created if missing.
func SaveGlobal(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("nil config")
	}
	log := slog.Default().With("concern", "process.daemon.config")
	globalDir := filepath.Dir(GlobalConfigPath())
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		log.Warn("config.save.mkdir_failed",
			"component", "config",
			"subcomponent", "save",
			"path", globalDir,
			"err", err,
		)
		return fmt.Errorf("create global config dir: %w", err)
	}
	tomlPath := filepath.Join(globalDir, "config.toml")
	tmp := tomlPath + ".tmp"
	encoded, err := toml.Marshal(cfg)
	if err != nil {
		log.Warn("config.save.encode_failed",
			"component", "config",
			"subcomponent", "save",
			"err", err,
		)
		return fmt.Errorf("encode toml: %w", err)
	}
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		log.Warn("config.save.write_tmp_failed",
			"component", "config",
			"subcomponent", "save",
			"path", tmp,
			"err", err,
		)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, tomlPath); err != nil {
		_ = os.Remove(tmp)
		log.Warn("config.save.rename_failed",
			"component", "config",
			"subcomponent", "save",
			"tmp", tmp,
			"path", tomlPath,
			"err", err,
		)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func NewConfigWithDefaults() *Config {
	cfg := NewConfig()
	_ = applyLoggingDefaultsAndValidate(cfg)
	_ = applyAdapterRetryDefaultsAndValidate(&cfg.Adapter.Retry)
	return cfg
}

const (
	defaultLoggingRotationMaxSizeMB  = 64
	defaultLoggingRotationMaxBackups = 192
	defaultLoggingRotationMaxAgeDays = 14
	defaultLoggingBodyMaxKB          = 32
)

func applyLoggingDefaultsAndValidate(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	logLevel := strings.ToLower(strings.TrimSpace(cfg.Logging.Level))
	if logLevel == "" {
		logLevel = "info"
	}
	cfg.Logging.Level = logLevel
	cfg.Logging.Paths.TUI = strings.TrimSpace(cfg.Logging.Paths.TUI)
	cfg.Logging.Paths.Daemon = strings.TrimSpace(cfg.Logging.Paths.Daemon)

	if cfg.Logging.Rotation.MaxSizeMB <= 0 {
		cfg.Logging.Rotation.MaxSizeMB = defaultLoggingRotationMaxSizeMB
	}
	if cfg.Logging.Rotation.Enabled == nil {
		v := true
		cfg.Logging.Rotation.Enabled = &v
	}
	if cfg.Logging.Rotation.MaxBackups < 0 {
		return fmt.Errorf("logging.rotation.max_backups must be >= 0")
	}
	if cfg.Logging.Rotation.MaxBackups == 0 {
		cfg.Logging.Rotation.MaxBackups = defaultLoggingRotationMaxBackups
	}
	if cfg.Logging.Rotation.MaxAgeDays < 0 {
		return fmt.Errorf("logging.rotation.max_age_days must be >= 0")
	}
	if cfg.Logging.Rotation.MaxAgeDays == 0 {
		cfg.Logging.Rotation.MaxAgeDays = defaultLoggingRotationMaxAgeDays
	}
	if cfg.Logging.Rotation.Compress == nil {
		v := true
		cfg.Logging.Rotation.Compress = &v
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.Logging.Body.Mode))
	if mode == "" {
		mode = "summary"
	}
	cfg.Logging.Body.Mode = mode

	if cfg.Logging.Body.MaxKB <= 0 {
		cfg.Logging.Body.MaxKB = defaultLoggingBodyMaxKB
	}
	if cfg.Logging.Body.MaxKB > 256 {
		return fmt.Errorf("logging.body.max_kb must be between 1 and 256")
	}
	switch cfg.Logging.Body.Mode {
	case "", "summary", "whitelist", "raw", "off":
	default:
		return fmt.Errorf("logging.body.mode must be one of summary|whitelist|raw|off")
	}
	if cfg.Logging.Body.Mode == "" {
		cfg.Logging.Body.Mode = "summary"
	}

	if cfg.Logging.Transcript.Enabled == nil {
		v := true
		cfg.Logging.Transcript.Enabled = &v
	}
	tmode := strings.ToLower(strings.TrimSpace(cfg.Logging.Transcript.Mode))
	if tmode == "" {
		tmode = "summary"
	}
	switch tmode {
	case "summary", "raw":
	default:
		return fmt.Errorf("logging.transcript.mode must be one of summary|raw")
	}
	cfg.Logging.Transcript.Mode = tmode

	if err := applyMITMDefaultsAndValidate(&cfg.MITM); err != nil {
		return err
	}

	cfg.Adapter.Codex.ReasoningSummary = normalizeCodexReasoningSummary(cfg.Adapter.Codex.ReasoningSummary)
	switch cfg.Adapter.Codex.ReasoningSummary {
	case "auto", "concise", "detailed", "none":
	default:
		return fmt.Errorf("adapter.codex.reasoning_summary must be one of auto|concise|detailed|none")
	}

	if err := applyAdapterNoticeDefaultsAndValidate(&cfg.Adapter.Notices); err != nil {
		return err
	}
	if err := applyAdapterRetryDefaultsAndValidate(&cfg.Adapter.Retry); err != nil {
		return err
	}

	return applyAdapterReasoningDefaultsAndValidate(&cfg.Adapter)
}

const codexOverloadedRetryPolicyName = "codex.responses.overloaded"

func applyAdapterRetryDefaultsAndValidate(retry *AdapterRetry) error {
	if retry == nil {
		return nil
	}
	retry.Policies = appendBuiltinAdapterRetryPolicies(retry.Policies)
	for i := range retry.Policies {
		if err := normalizeAdapterRetryPolicy(&retry.Policies[i]); err != nil {
			return err
		}
	}
	return nil
}

func appendBuiltinAdapterRetryPolicies(policies []AdapterRetryPolicy) []AdapterRetryPolicy {
	for _, policy := range policies {
		if strings.TrimSpace(policy.Name) == codexOverloadedRetryPolicyName {
			return policies
		}
	}
	return append(policies, builtinCodexOverloadedRetryPolicy())
}

func builtinCodexOverloadedRetryPolicy() AdapterRetryPolicy {
	enabled := true
	return AdapterRetryPolicy{
		Name:                     codexOverloadedRetryPolicyName,
		Enabled:                  &enabled,
		MaxAttempts:              3,
		InitialDelay:             AdapterRetryDuration(250 * time.Millisecond),
		MaxDelay:                 AdapterRetryDuration(2 * time.Second),
		Multiplier:               2,
		JitterFraction:           0.2,
		RetryWhenResponseStarted: false,
		Match: AdapterRetryMatchers{
			Backends:          []string{"codex"},
			Operations:        []string{"codex.responses.websocket.generate"},
			ErrorClasses:      []string{"scanner_error", "websocket_error", "response_failed"},
			MessageSubstrings: []string{"Our servers are currently overloaded. Please try again later."},
		},
	}
}

func normalizeAdapterRetryPolicy(policy *AdapterRetryPolicy) error {
	policy.Name = strings.TrimSpace(policy.Name)
	if policy.Name == "" {
		return fmt.Errorf("adapter.retry.policies contains a policy without name")
	}
	if policy.Enabled == nil {
		enabled := true
		policy.Enabled = &enabled
	}
	if policy.MaxAttempts < 1 {
		return fmt.Errorf("adapter.retry.policies.%s.max_attempts must be at least 1", policy.Name)
	}
	if policy.InitialDelay.Duration() < 0 {
		return fmt.Errorf("adapter.retry.policies.%s.initial_delay must be non-negative", policy.Name)
	}
	if policy.MaxDelay.Duration() < 0 {
		return fmt.Errorf("adapter.retry.policies.%s.max_delay must be non-negative", policy.Name)
	}
	if policy.MaxDelay.Duration() > 0 && policy.InitialDelay.Duration() > policy.MaxDelay.Duration() {
		return fmt.Errorf("adapter.retry.policies.%s.initial_delay must be less than or equal to max_delay", policy.Name)
	}
	if policy.Multiplier < 0 {
		return fmt.Errorf("adapter.retry.policies.%s.multiplier must be non-negative", policy.Name)
	}
	if policy.JitterFraction < 0 || policy.JitterFraction > 1 {
		return fmt.Errorf("adapter.retry.policies.%s.jitter_fraction must be between 0 and 1", policy.Name)
	}
	normalizeStringSlice(&policy.Match.Backends)
	normalizeStringSlice(&policy.Match.Operations)
	normalizeStringSlice(&policy.Match.ErrorClasses)
	normalizeStringSlice(&policy.Match.ErrorCodes)
	normalizeStringSlice(&policy.Match.MessageSubstrings)
	return nil
}

func normalizeStringSlice(values *[]string) {
	if values == nil {
		return
	}
	out := make([]string, 0, len(*values))
	for _, value := range *values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	*values = out
}

func applyAdapterNoticeDefaultsAndValidate(notices *AdapterNotices) error {
	if notices == nil {
		return nil
	}
	thresholds, err := normalizeNoticeUsageThresholds(notices.Usage.ThresholdsUsedPercent)
	if err != nil {
		return err
	}
	notices.Usage.ThresholdsUsedPercent = thresholds
	policy, err := normalizeNoticeUsageRepeatPolicy(notices.Usage.Repeat)
	if err != nil {
		return err
	}
	notices.Usage.Repeat = policy
	return nil
}

func applyMITMDefaultsAndValidate(mitm *MITMConfig) error {
	if mitm == nil {
		return nil
	}
	providers, err := parseMITMProviders(mitm.Providers)
	if err != nil {
		return err
	}
	mitm.Providers = providers

	mitm.BodyMode = normalizeMITMBodyMode(mitm.BodyMode)
	switch mitm.BodyMode {
	case "summary", "raw", "off":
	default:
		return fmt.Errorf("mitm.body_mode must be one of summary|raw|off")
	}

	mitm.CaptureDir = strings.TrimSpace(mitm.CaptureDir)
	if mitm.CaptureDir == "" {
		mitm.CaptureDir = filepath.Join(DefaultStateDir(), "mitm")
	}
	captureRules, err := normalizeMITMCaptureRouteRules(mitm.CaptureRules)
	if err != nil {
		return err
	}
	mitm.CaptureRules = captureRules
	return nil
}

func DefaultMITMCaptureRouteRules() []MITMCaptureRouteRule {
	return []MITMCaptureRouteRule{
		{
			Concern:   "cursor.bidi",
			Provider:  "cursor",
			Method:    "POST",
			PathExact: "/aiserver.v1.AiService/BidiAppend",
		},
		{
			Concern:      "cursor.agent",
			Provider:     "cursor",
			PathContains: "AiService",
		},
		{
			Concern:      "cursor.catalog",
			Provider:     "cursor",
			PathContains: "Model",
		},
		{
			Concern:      "cursor.account",
			Provider:     "cursor",
			PathContains: "User",
		},
		{
			Concern:      "cursor.telemetry",
			Provider:     "cursor",
			Host:         "telemetry.cursor.com",
			PathContains: "telemetry",
		},
		{
			Concern:      "cursor.filesync",
			Provider:     "cursor",
			PathContains: "FileSync",
		},
		{
			Concern: "unknown",
		},
	}
}

func normalizeMITMCaptureRouteRules(rules []MITMCaptureRouteRule) ([]MITMCaptureRouteRule, error) {
	if len(rules) == 0 {
		return DefaultMITMCaptureRouteRules(), nil
	}
	normalizedRules := make([]MITMCaptureRouteRule, 0, len(rules))
	for i, rule := range rules {
		normalizedRule, err := normalizeMITMCaptureRouteRule(rule)
		if err != nil {
			return nil, fmt.Errorf("mitm.capture_rules[%d]: %w", i, err)
		}
		normalizedRules = append(normalizedRules, normalizedRule)
	}
	return normalizedRules, nil
}

func normalizeMITMCaptureRouteRule(rule MITMCaptureRouteRule) (MITMCaptureRouteRule, error) {
	rule.Concern = strings.TrimSpace(rule.Concern)
	if rule.Concern == "" {
		return MITMCaptureRouteRule{}, fmt.Errorf("concern is required")
	}
	rule.Provider = normalizeMITMProviderName(rule.Provider)
	if rule.Provider != "" && !isValidMITMProviderName(rule.Provider) {
		return MITMCaptureRouteRule{}, fmt.Errorf("provider %q is invalid", rule.Provider)
	}
	rule.Host = strings.Trim(strings.ToLower(strings.TrimSpace(rule.Host)), ".")
	rule.Method = strings.ToUpper(strings.TrimSpace(rule.Method))
	rule.PathExact = strings.TrimSpace(rule.PathExact)
	rule.PathPrefix = strings.TrimSpace(rule.PathPrefix)
	rule.PathContains = strings.TrimSpace(rule.PathContains)
	rule.ContentTypeContains = strings.ToLower(strings.TrimSpace(rule.ContentTypeContains))
	return rule, nil
}

// applyAdapterReasoningDefaultsAndValidate validates the per-provider
// [adapter.anthropic.reasoning] and [adapter.codex.reasoning] blocks and
// folds the legacy [adapter.synthetic_content] inbound materialization
// values forward when the new blocks are unset. The legacy path stays
// readable for one release; each forwarded value emits a single startup
// WARN so operators see the migration prompt without log spam.
func applyAdapterReasoningDefaultsAndValidate(adapter *AdapterConfig) error {
	if adapter == nil {
		return nil
	}
	log := slog.Default().With("concern", "adapter.reasoning.config")

	legacyAnthropic := strings.TrimSpace(string(adapter.SyntheticContent.Anthropic.InboundThinkingMaterialization))
	if adapter.Anthropic.Reasoning.InboundThinking == "" && legacyAnthropic != "" {
		adapter.Anthropic.Reasoning.InboundThinking = AnthropicInboundThinking(legacyAnthropic)
		log.Warn("adapter.reasoning.legacy_synthetic_content_forwarded",
			"component", "config",
			"subcomponent", "adapter_reasoning",
			"provider", "anthropic",
			"legacy_field", "adapter.synthetic_content.anthropic.inbound_thinking_materialization",
			"new_field", "adapter.anthropic.reasoning.inbound_thinking",
			"value", legacyAnthropic,
			"reason", "legacy synthetic_content block is deprecated; copy the value into the new block",
		)
	}
	switch adapter.Anthropic.Reasoning.InboundThinking {
	case "",
		AnthropicInboundThinkingNative,
		AnthropicInboundThinkingDrop,
		AnthropicInboundThinkingPlainText,
		AnthropicInboundThinkingPassthrough:
	default:
		return fmt.Errorf("adapter.anthropic.reasoning.inbound_thinking must be one of native_thinking_block|drop|plain_text_concat|passthrough (got %q)", adapter.Anthropic.Reasoning.InboundThinking)
	}

	legacyCodex := strings.TrimSpace(string(adapter.SyntheticContent.Codex.InboundThinkingMaterialization))
	if adapter.Codex.Reasoning.RoundTripSummary == "" && legacyCodex != "" {
		adapter.Codex.Reasoning.RoundTripSummary = CodexRoundTripSummary(legacyCodex)
		log.Warn("adapter.reasoning.legacy_synthetic_content_forwarded",
			"component", "config",
			"subcomponent", "adapter_reasoning",
			"provider", "codex",
			"legacy_field", "adapter.synthetic_content.codex.inbound_thinking_materialization",
			"new_field", "adapter.codex.reasoning.round_trip_summary",
			"value", legacyCodex,
			"reason", "legacy synthetic_content block is deprecated; copy the value into the new block",
		)
	}
	switch adapter.Codex.Reasoning.RoundTripSummary {
	case "",
		CodexRoundTripSummaryNative,
		CodexRoundTripSummaryDrop,
		CodexRoundTripSummaryPlainText:
	default:
		return fmt.Errorf("adapter.codex.reasoning.round_trip_summary must be one of native_summary_field|drop|plain_text_concat (got %q)", adapter.Codex.Reasoning.RoundTripSummary)
	}

	switch adapter.Codex.Reasoning.RoundTripEncrypted {
	case "",
		CodexRoundTripEncryptedRoundTrip,
		CodexRoundTripEncryptedDrop:
	default:
		return fmt.Errorf("adapter.codex.reasoning.round_trip_encrypted must be one of round_trip|drop (got %q)", adapter.Codex.Reasoning.RoundTripEncrypted)
	}

	return nil
}

const mitmProviderAll = "all"

func parseMITMProviders(providers MITMProviderSet) (MITMProviderSet, error) {
	if len(providers) == 0 {
		return MITMProviderSet{mitmProviderAll}, nil
	}
	seenProviders := make(map[string]bool, len(providers))
	normalizedProviders := make(MITMProviderSet, 0, len(providers))
	hasAll := false
	for _, providerList := range providers {
		for _, provider := range splitMITMProviderList(providerList) {
			normalizedProvider := normalizeMITMProviderName(provider)
			if normalizedProvider == mitmProviderAll {
				hasAll = true
				continue
			}
			if !isValidMITMProviderName(normalizedProvider) {
				return nil, fmt.Errorf("mitm.providers contains invalid provider %q", provider)
			}
			if seenProviders[normalizedProvider] {
				continue
			}
			seenProviders[normalizedProvider] = true
			normalizedProviders = append(normalizedProviders, normalizedProvider)
		}
	}
	if hasAll {
		if len(normalizedProviders) > 0 {
			return nil, fmt.Errorf("mitm.providers cannot combine \"all\" with explicit providers")
		}
		return MITMProviderSet{mitmProviderAll}, nil
	}
	if len(normalizedProviders) == 0 {
		return MITMProviderSet{mitmProviderAll}, nil
	}
	slices.Sort(normalizedProviders)
	return normalizedProviders, nil
}

func normalizeMITMProviders(providers MITMProviderSet) MITMProviderSet {
	normalizedProviders, err := parseMITMProviders(providers)
	if err != nil {
		return providers
	}
	return normalizedProviders
}

func parseMITMProviderControlValue(value string) (MITMProviderSet, error) {
	return parseMITMProviders(MITMProviderSet{value})
}

func formatMITMProviders(providers MITMProviderSet) string {
	return strings.Join(normalizeMITMProviders(providers), ",")
}

func splitMITMProviderList(providerList string) []string {
	return strings.FieldsFunc(providerList, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

func normalizeMITMProviderName(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func isValidMITMProviderName(provider string) bool {
	if provider == "" || provider == mitmProviderAll {
		return false
	}
	for _, r := range provider {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func normalizeMITMBodyMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "summary":
		return "summary"
	case "raw":
		return "raw"
	case "off":
		return "off"
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}

func normalizeCodexReasoningSummary(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "auto":
		return "auto"
	case "concise":
		return "concise"
	case "detailed":
		return "detailed"
	case "none":
		return "none"
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}

func normalizeNoticeUsageThresholds(raw []float64) ([]float64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	thresholds := make([]float64, 0, len(raw))
	for _, threshold := range raw {
		if threshold <= 0 || threshold >= 100 {
			return nil, fmt.Errorf("adapter.notices.usage.thresholds_used_percent values must be between 0 and 100")
		}
		thresholds = append(thresholds, threshold)
	}
	slices.Sort(thresholds)
	out := thresholds[:0]
	var previous float64
	for index, threshold := range thresholds {
		if index > 0 && threshold == previous {
			continue
		}
		out = append(out, threshold)
		previous = threshold
	}
	return append([]float64(nil), out...), nil
}

func normalizeNoticeUsageRepeatPolicy(raw AdapterNoticeRepeatPolicy) (AdapterNoticeRepeatPolicy, error) {
	mode := AdapterNoticeRepeatMode(strings.ToLower(strings.TrimSpace(string(raw.Mode))))
	if mode == "" {
		mode = AdapterNoticeRepeatEvery
	}
	out := AdapterNoticeRepeatPolicy{Mode: mode}
	switch mode {
	case AdapterNoticeRepeatEvery, AdapterNoticeRepeatOncePerThresholdUntilReset:
		return out, nil
	case AdapterNoticeRepeatTimeCooldown:
		cooldown, err := time.ParseDuration(strings.TrimSpace(raw.Cooldown))
		if err != nil || cooldown <= 0 {
			return AdapterNoticeRepeatPolicy{}, fmt.Errorf("adapter.notices.usage.repeat.cooldown must be a positive duration when mode is time_cooldown")
		}
		out.Cooldown = strings.TrimSpace(raw.Cooldown)
		out.CooldownDuration = cooldown
		return out, nil
	case AdapterNoticeRepeatTurnCooldown:
		if raw.CooldownTurns <= 0 {
			return AdapterNoticeRepeatPolicy{}, fmt.Errorf("adapter.notices.usage.repeat.cooldown_turns must be positive when mode is turn_cooldown")
		}
		out.CooldownTurns = raw.CooldownTurns
		return out, nil
	default:
		return AdapterNoticeRepeatPolicy{}, fmt.Errorf("adapter.notices.usage.repeat.mode must be one of every|once_per_threshold_until_reset|time_cooldown|turn_cooldown")
	}
}

// MergedProfiles helper removed; callers now use LoadGlobalOrDefault and project
// config loading inline at their callsites.
