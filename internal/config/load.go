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
	if cfg.Logging.Transcript.MaxAgeDays < 0 {
		return fmt.Errorf("logging.transcript.max_age_days must be >= 0")
	}
	if cfg.Logging.Transcript.MaxChats < 0 {
		return fmt.Errorf("logging.transcript.max_chats must be >= 0")
	}
	if cfg.Logging.Transcript.Enabled != nil && *cfg.Logging.Transcript.Enabled {
		if cfg.Logging.Transcript.MaxAgeDays == 0 || cfg.Logging.Transcript.MaxChats == 0 {
			slog.Warn("config.logging.transcript.disabled_missing_retention",
				"component", "config",
				"max_age_days", cfg.Logging.Transcript.MaxAgeDays,
				"max_chats", cfg.Logging.Transcript.MaxChats,
				"reason", "transcript router requires positive max_age_days and max_chats; feature off",
			)
		}
	}

	cfg.MITM.Providers = normalizeMITMProviders(cfg.MITM.Providers)
	switch cfg.MITM.Providers {
	case "both", "claude", "codex":
	default:
		return fmt.Errorf("mitm.providers must be one of both|claude|codex")
	}

	cfg.MITM.BodyMode = normalizeMITMBodyMode(cfg.MITM.BodyMode)
	switch cfg.MITM.BodyMode {
	case "summary", "raw", "off":
	default:
		return fmt.Errorf("mitm.body_mode must be one of summary|raw|off")
	}

	cfg.MITM.CaptureDir = strings.TrimSpace(cfg.MITM.CaptureDir)
	if cfg.MITM.CaptureDir == "" {
		cfg.MITM.CaptureDir = filepath.Join(DefaultStateDir(), "mitm")
	}

	cfg.Adapter.Codex.ReasoningSummary = normalizeCodexReasoningSummary(cfg.Adapter.Codex.ReasoningSummary)
	switch cfg.Adapter.Codex.ReasoningSummary {
	case "auto", "concise", "detailed", "none":
	default:
		return fmt.Errorf("adapter.codex.reasoning_summary must be one of auto|concise|detailed|none")
	}

	thresholds, err := normalizeNoticeUsageThresholds(cfg.Adapter.Notices.Usage.ThresholdsUsedPercent)
	if err != nil {
		return err
	}
	cfg.Adapter.Notices.Usage.ThresholdsUsedPercent = thresholds
	policy, err := normalizeNoticeUsageRepeatPolicy(cfg.Adapter.Notices.Usage.Repeat)
	if err != nil {
		return err
	}
	cfg.Adapter.Notices.Usage.Repeat = policy

	return applyAdapterReasoningDefaultsAndValidate(&cfg.Adapter)
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

func normalizeMITMProviders(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "both", "all":
		return "both"
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
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
