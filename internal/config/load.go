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

	"goodkind.io/clyde/internal/adapter/anthropic/anthmode"
	"goodkind.io/clyde/internal/util"
)

// loadConfig tries to load config.toml from a directory.
// Uses pelletier/go-toml/v2; the older BurntSushi/toml dep is now unmaintained
// and was removed. Pelletier mirrors the same Marshal / Unmarshal API surface
// so the migration is a one-line import swap on each call.
func loadConfig(dir string) (*Config, error) {
	log := slog.Default().With("concern", "process.daemon.config")
	tomlPath := filepath.Join(dir, "config.toml")
	if !util.FileExists(tomlPath) {
		return nil, os.ErrNotExist
	}

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
	warnRemovedLoggingConfig(data, tomlPath, log)
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

type removedLoggingConfig struct {
	Logging removedLoggingSection `toml:"logging"`
	MITM    removedMITMSection    `toml:"mitm"`
}

type removedLoggingSection struct {
	Body    *removedLoggingBody   `toml:"body"`
	Cleanup removedLoggingCleanup `toml:"cleanup"`
}

type removedLoggingBody struct {
	Mode  *string `toml:"mode"`
	MaxKB *int    `toml:"max_kb"`
}

type removedLoggingCleanup struct {
	AuditOnly   *bool   `toml:"audit_only"`
	CleanupMode *string `toml:"cleanup_mode"`
}

type removedMITMSection struct {
	BodyMode *string `toml:"body_mode"`
}

// warnRemovedLoggingConfig surfaces a clear slog warning for every
// removed logging key that still appears in the operator's config. The
// intent is to make the hard cut visible without blocking startup:
// existing operator configs predate the refactor, and forcing them to
// edit a file before `clyde` can run again is a worse failure mode
// than ignoring the stale section. Operators see one warning per
// removed key so the migration path is obvious in the daemon log, and
// the values themselves have no effect because the corresponding
// config structs no longer exist.
func warnRemovedLoggingConfig(data []byte, tomlPath string, log *slog.Logger) {
	removedConfig, err := unmarshalRemovedLoggingConfig(data)
	if err != nil {
		return
	}
	emitRemovedLoggingKeyWarning := func(key string) {
		log.Warn("config.load.removed_logging_key",
			"component", "config",
			"subcomponent", "load",
			"path", tomlPath,
			"format", "toml",
			"removed_key", key,
		)
	}
	if removedConfig.Logging.Body != nil {
		emitRemovedLoggingKeyWarning("logging.body")
	}
	if removedConfig.MITM.BodyMode != nil {
		emitRemovedLoggingKeyWarning("mitm.body_mode")
	}
	if removedConfig.Logging.Cleanup.AuditOnly != nil {
		emitRemovedLoggingKeyWarning("logging.cleanup.audit_only")
	}
	if removedConfig.Logging.Cleanup.CleanupMode != nil {
		emitRemovedLoggingKeyWarning("logging.cleanup.cleanup_mode")
	}
}

func unmarshalRemovedLoggingConfig(data []byte) (removedLoggingConfig, error) {
	var removedConfig removedLoggingConfig
	if err := toml.Unmarshal(data, &removedConfig); err != nil {
		slog.Warn("config.load.removed_logging_config_scan_failed",
			"component", "config",
			"subcomponent", "load",
			"format", "toml",
			"err", err,
		)
		return removedLoggingConfig{}, fmt.Errorf("scan removed logging config: %w", err)
	}
	return removedConfig, nil
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
)

var knownLoggingSinkNames = map[string]bool{
	LoggingSinkDaemon:           true,
	LoggingSinkTUI:              true,
	LoggingSinkCodexSidecar:     true,
	LoggingSinkAnthropicSidecar: true,
	LoggingSinkAudit:            true,
	LoggingSinkConcerns:         true,
	LoggingSinkTranscripts:      true,
	LoggingSinkMITMCapture:      true,
	LoggingSinkMITMRaw:          true,
	LoggingSinkInventory:        true,
}

func applyLoggingDefaultsAndValidate(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	applySessionDefaults(&cfg.Defaults)
	if err := applyLoggingCoreDefaults(&cfg.Logging); err != nil {
		return err
	}
	if err := applyLoggingTranscriptDefaults(&cfg.Logging.Transcript); err != nil {
		return err
	}
	if err := normalizeLoggingSinks(&cfg.Logging.Sinks); err != nil {
		return err
	}
	if err := normalizeLoggingRequest(&cfg.Logging.Request); err != nil {
		return err
	}
	if err := normalizeLoggingInventory(&cfg.Logging.Inventory); err != nil {
		return err
	}
	if err := normalizeLoggingConcernOverrides(cfg.Logging.Concerns); err != nil {
		return err
	}

	rawCaptureEnabled := false
	if cfg.Logging.RawCapture.Enabled != nil {
		rawCaptureEnabled = *cfg.Logging.RawCapture.Enabled
	}
	cfg.MITM.RawCaptureEnabled = rawCaptureEnabled

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

	applyAutoNameDefaults(&cfg.AutoName)

	return applyAdapterReasoningDefaultsAndValidate(&cfg.Adapter)
}

func applySessionDefaults(defaults *Defaults) {
	if strings.TrimSpace(defaults.CompactCounter) == "" {
		defaults.CompactCounter = "api"
	}
}

const (
	defaultAutoNameMaxCallsPerHour      = 6
	defaultAutoNameCooldown             = 30 * time.Minute
	defaultAutoNameMinUserMessages      = 3
	defaultAutoNameMinDigitRunForRedact = 7
)

// applyAutoNameDefaults fills in absent or zero AutoName fields with
// the canonical defaults documented on the AutoNameConfig type. The
// pointer-bool fields preserve the distinction between "operator
// explicitly set false" and "absent" so a partial [autoname] block
// merges defaults for the missing fields only.
func applyAutoNameDefaults(autoname *AutoNameConfig) {
	if autoname == nil {
		return
	}
	if autoname.Enabled == nil {
		enabled := true
		autoname.Enabled = &enabled
	}
	if autoname.MaxCallsPerHour == 0 {
		autoname.MaxCallsPerHour = defaultAutoNameMaxCallsPerHour
	}
	if autoname.Cooldown == 0 {
		autoname.Cooldown = Duration(defaultAutoNameCooldown)
	}
	if autoname.MinUserMessages == 0 {
		autoname.MinUserMessages = defaultAutoNameMinUserMessages
	}
	applyAutoNameRedactDefaults(&autoname.Redact)
}

func applyAutoNameRedactDefaults(redact *RedactPolicy) {
	if redact == nil {
		return
	}
	if redact.MinDigitRunForRedact == 0 {
		redact.MinDigitRunForRedact = defaultAutoNameMinDigitRunForRedact
	}
	if redact.StripEmails == nil {
		strip := true
		redact.StripEmails = &strip
	}
	if redact.StripPaths == nil {
		strip := true
		redact.StripPaths = &strip
	}
	if redact.StripKeyPrefixes == nil {
		strip := true
		redact.StripKeyPrefixes = &strip
	}
}

func applyLoggingCoreDefaults(logging *LoggingConfig) error {
	logLevel := strings.ToLower(strings.TrimSpace(logging.Level))
	if logLevel == "" {
		logLevel = "info"
	}
	logging.Level = logLevel
	logging.Paths.TUI = strings.TrimSpace(logging.Paths.TUI)
	logging.Paths.Daemon = strings.TrimSpace(logging.Paths.Daemon)
	if err := validateLoggingLevel("logging.level", logging.Level); err != nil {
		return err
	}
	if err := validateLoggingRotation("logging.rotation", logging.Rotation); err != nil {
		return err
	}
	if logging.Rotation.MaxSizeMB == 0 {
		logging.Rotation.MaxSizeMB = defaultLoggingRotationMaxSizeMB
	}
	if logging.Rotation.Enabled == nil {
		v := true
		logging.Rotation.Enabled = &v
	}
	if logging.Rotation.MaxBackups == 0 {
		logging.Rotation.MaxBackups = defaultLoggingRotationMaxBackups
	}
	if logging.Rotation.MaxAgeDays == 0 {
		logging.Rotation.MaxAgeDays = defaultLoggingRotationMaxAgeDays
	}
	if logging.Rotation.Compress == nil {
		v := true
		logging.Rotation.Compress = &v
	}
	if logging.Cleanup.Enabled == nil {
		v := true
		logging.Cleanup.Enabled = &v
	}
	if logging.RawCapture.Enabled == nil {
		v := false
		logging.RawCapture.Enabled = &v
	}
	return validateLoggingCleanup("logging.cleanup", logging.Cleanup)
}

func validateLoggingCleanup(path string, cleanup LoggingCleanup) error {
	if cleanup.MaxAgeDays != nil && *cleanup.MaxAgeDays < 0 {
		return fmt.Errorf("%s.max_age_days must be >= 0", path)
	}
	if cleanup.MaxBackups != nil && *cleanup.MaxBackups < 0 {
		return fmt.Errorf("%s.max_backups must be >= 0", path)
	}
	if cleanup.MaxTotalMB != nil && *cleanup.MaxTotalMB < 0 {
		return fmt.Errorf("%s.max_total_mb must be >= 0", path)
	}
	return nil
}

func applyLoggingTranscriptDefaults(transcript *LoggingTranscript) error {
	if transcript.Enabled == nil {
		v := true
		transcript.Enabled = &v
	}
	return nil
}

func normalizeLoggingSinks(sinks *LoggingSinks) error {
	if sinks == nil {
		return nil
	}
	normalizedEnabled := make([]string, 0, len(sinks.Enabled))
	seen := make(map[string]bool, len(sinks.Enabled))
	for _, sinkName := range sinks.Enabled {
		normalizedName := strings.ToLower(strings.TrimSpace(sinkName))
		if normalizedName == "" {
			return fmt.Errorf("logging.sinks.enabled contains an empty sink name")
		}
		if !knownLoggingSinkNames[normalizedName] {
			return fmt.Errorf("logging.sinks.enabled contains unknown sink %q", sinkName)
		}
		if seen[normalizedName] {
			continue
		}
		seen[normalizedName] = true
		normalizedEnabled = append(normalizedEnabled, normalizedName)
	}
	sinks.Enabled = normalizedEnabled
	return nil
}

func normalizeLoggingRequest(request *LoggingRequest) error {
	if request == nil {
		return nil
	}
	policy := strings.ToLower(strings.TrimSpace(request.IncompletePolicy))
	if policy == "" {
		policy = "warn"
	}
	switch policy {
	case "warn", "fail_test":
		request.IncompletePolicy = policy
	default:
		return fmt.Errorf("logging.request.incomplete_policy must be one of warn|fail_test")
	}
	for surface, legs := range request.RequiredLegs {
		trimmedSurface := strings.TrimSpace(surface)
		if trimmedSurface == "" {
			return fmt.Errorf("logging.request.required_legs contains an empty surface")
		}
		for _, leg := range legs {
			if strings.TrimSpace(leg) == "" {
				return fmt.Errorf("logging.request.required_legs.%s contains an empty leg", surface)
			}
		}
	}
	return nil
}

func normalizeLoggingInventory(inventory *LoggingInventory) error {
	if inventory == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(inventory.Mode))
	if mode == "" {
		mode = "indexed"
	}
	switch mode {
	case "indexed", "deep":
		inventory.Mode = mode
		return nil
	default:
		return fmt.Errorf("logging.inventory.mode must be one of indexed|deep")
	}
}

func normalizeLoggingConcernOverrides(concerns LoggingConcerns) error {
	for concernName, concern := range concerns {
		trimmedName := strings.TrimSpace(concernName)
		if trimmedName == "" {
			return fmt.Errorf("logging.concerns contains an empty concern name")
		}
		if err := normalizeLoggingControl("logging.concerns."+concernName, concern.Level, concern.Detail); err != nil {
			return err
		}
		if err := validateLoggingRotation("logging.concerns."+concernName+".rotation", concern.Rotation); err != nil {
			return err
		}
		if sinkName := strings.ToLower(strings.TrimSpace(concern.Sink)); sinkName != "" {
			if !knownLoggingSinkNames[sinkName] {
				return fmt.Errorf("logging.concerns.%s.sink is not a known sink", concernName)
			}
			concern.Sink = sinkName
		}
		concern.Level = normalizeLoggingOptionalValue(concern.Level)
		concern.Detail = normalizeLoggingOptionalValue(concern.Detail)
		concerns[concernName] = concern
	}
	return nil
}

func normalizeLoggingControl(path string, level string, detail string) error {
	if err := validateLoggingLevel(path+".level", normalizeLoggingOptionalValue(level)); err != nil {
		return err
	}
	switch normalizeLoggingOptionalValue(detail) {
	case "", "summary", "verbose":
		return nil
	default:
		return fmt.Errorf("%s.detail must be one of summary|verbose", path)
	}
}

func validateLoggingLevel(path string, level string) error {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("%s must be one of debug|info|warn|error", path)
	}
}

func validateLoggingRotation(path string, rotation LoggingRotation) error {
	if rotation.MaxSizeMB < 0 {
		return fmt.Errorf("%s.max_size_mb must be >= 0", path)
	}
	if rotation.MaxBackups < 0 {
		return fmt.Errorf("%s.max_backups must be >= 0", path)
	}
	if rotation.MaxAgeDays < 0 {
		return fmt.Errorf("%s.max_age_days must be >= 0", path)
	}
	return nil
}

func normalizeLoggingOptionalValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// applyAdapterRetryDefaultsAndValidate normalizes and validates
// operator-supplied retry policies. Provider-specific builtin policies
// are not injected here; each provider package owns and appends its own
// builtins at construction so provider knowledge stays out of the
// generic config layer.
func applyAdapterRetryDefaultsAndValidate(retry *AdapterRetry) error {
	if retry == nil {
		return nil
	}
	for i := range retry.Policies {
		if err := normalizeAdapterRetryPolicy(&retry.Policies[i]); err != nil {
			return err
		}
	}
	return nil
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
	if policy.InitialDelay.AsDuration() < 0 {
		return fmt.Errorf("adapter.retry.policies.%s.initial_delay must be non-negative", policy.Name)
	}
	if policy.MaxDelay.AsDuration() < 0 {
		return fmt.Errorf("adapter.retry.policies.%s.max_delay must be non-negative", policy.Name)
	}
	if policy.MaxDelay.AsDuration() > 0 && policy.InitialDelay.AsDuration() > policy.MaxDelay.AsDuration() {
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

	mitm.CaptureDir = normalizeMITMCaptureDir(strings.TrimSpace(mitm.CaptureDir))
	if mitm.CaptureDir == "" {
		mitm.CaptureDir = filepath.Join(DefaultStateDir(), "mitm")
	}
	if err := validateLoggingRotation("mitm.capture.rotation", mitm.Capture.Rotation); err != nil {
		return err
	}
	captureRules, err := normalizeMITMCaptureRouteRules(mitm.CaptureRules)
	if err != nil {
		return err
	}
	mitm.CaptureRules = captureRules
	if err := applyMITMListenDefaultsAndValidate(&mitm.Listen); err != nil {
		return err
	}
	if err := applyMITMCADefaultsAndValidate(&mitm.CA); err != nil {
		return err
	}
	return nil
}

func normalizeMITMCaptureDir(captureDir string) string {
	if captureDir == "" {
		return ""
	}
	cleanCaptureDir := filepath.Clean(captureDir)
	if filepath.Base(cleanCaptureDir) == "always-on" {
		return filepath.Dir(cleanCaptureDir)
	}
	return captureDir
}

const (
	defaultMITMListenHost = "[::1]"
	defaultMITMListenPort = 48723
)

func applyMITMListenDefaultsAndValidate(listen *MITMListenConfig) error {
	listen.Host = strings.TrimSpace(listen.Host)
	if listen.Host == "" {
		listen.Host = defaultMITMListenHost
	}
	if listen.Port == 0 {
		listen.Port = defaultMITMListenPort
		return nil
	}
	if listen.Port < 0 || listen.Port > 65535 {
		return fmt.Errorf("mitm.listen.port %d is outside the valid TCP port range 1-65535", listen.Port)
	}
	return nil
}

func applyMITMCADefaultsAndValidate(ca *MITMCAConfig) error {
	caDir := filepath.Join(DefaultStateDir(), "mitm", "ca")

	ca.CertPath = strings.TrimSpace(ca.CertPath)
	if ca.CertPath == "" {
		ca.CertPath = filepath.Join(caDir, "clyde-mitm-ca.crt")
	} else {
		ca.CertPath = expandHome(ca.CertPath)
	}
	if !filepath.IsAbs(ca.CertPath) {
		return fmt.Errorf("mitm.ca.cert_path %q must be an absolute path after expansion", ca.CertPath)
	}

	ca.KeyPath = strings.TrimSpace(ca.KeyPath)
	if ca.KeyPath == "" {
		ca.KeyPath = filepath.Join(caDir, "clyde-mitm-ca.key")
	} else {
		ca.KeyPath = expandHome(ca.KeyPath)
	}
	if !filepath.IsAbs(ca.KeyPath) {
		return fmt.Errorf("mitm.ca.key_path %q must be an absolute path after expansion", ca.KeyPath)
	}
	return nil
}

// DefaultMITMCaptureRouteRules returns Clyde's built-in MITM concern routing.
func DefaultMITMCaptureRouteRules() []MITMCaptureRouteRule {
	return []MITMCaptureRouteRule{
		{
			Concern:             "cursor.bidi",
			Provider:            "cursor",
			Host:                "",
			Method:              "POST",
			PathExact:           "/aiserver.v1.AiService/BidiAppend",
			PathPrefix:          "",
			PathContains:        "",
			ContentTypeContains: "",
		},
		{
			Concern:             "cursor.agent",
			Provider:            "cursor",
			Host:                "",
			Method:              "",
			PathExact:           "",
			PathPrefix:          "",
			PathContains:        "AiService",
			ContentTypeContains: "",
		},
		{
			Concern:             "cursor.catalog",
			Provider:            "cursor",
			Host:                "",
			Method:              "",
			PathExact:           "",
			PathPrefix:          "",
			PathContains:        "Model",
			ContentTypeContains: "",
		},
		{
			Concern:             "cursor.account",
			Provider:            "cursor",
			Host:                "",
			Method:              "",
			PathExact:           "",
			PathPrefix:          "",
			PathContains:        "User",
			ContentTypeContains: "",
		},
		{
			Concern:             "cursor.telemetry",
			Provider:            "cursor",
			Host:                "telemetry.cursor.com",
			Method:              "",
			PathExact:           "",
			PathPrefix:          "",
			PathContains:        "telemetry",
			ContentTypeContains: "",
		},
		{
			Concern:             "cursor.filesync",
			Provider:            "cursor",
			Host:                "",
			Method:              "",
			PathExact:           "",
			PathPrefix:          "",
			PathContains:        "FileSync",
			ContentTypeContains: "",
		},
		{
			Concern:             "unknown",
			Provider:            "",
			Host:                "",
			Method:              "",
			PathExact:           "",
			PathPrefix:          "",
			PathContains:        "",
			ContentTypeContains: "",
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
			slog.Warn("config.mitm.capture_rule_invalid",
				"concern", "process.daemon.config",
				"component", "config",
				"subcomponent", "mitm",
				"index", i,
				"err", err,
			)
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
		adapter.Anthropic.Reasoning.InboundThinking = anthmode.InboundThinking(legacyAnthropic)
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
	if err := adapter.Anthropic.Reasoning.InboundThinking.Validate(); err != nil {
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
