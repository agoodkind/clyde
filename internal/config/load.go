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
	log := slog.Default()
	tomlPath := filepath.Join(dir, "config.toml")
	if !util.FileExists(tomlPath) {
		return nil, os.ErrNotExist
	}

	var cfg Config
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		log.Warn("config.load.read_failed", "concern", "config", "component", "config",
			"subcomponent", "load",
			"path", tomlPath,
			"format", "toml",
			"err", err,
		)
		return nil, fmt.Errorf("failed to read %s: %w", tomlPath, err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		log.Warn("config.load.parse_failed", "concern", "config", "component", "config",
			"subcomponent", "load",
			"path", tomlPath,
			"format", "toml",
			"err", err,
		)
		return nil, fmt.Errorf("failed to parse %s: %w", tomlPath, err)
	}
	pruneEmptyModelDeclarations(&cfg.Adapter)
	warnRemovedLoggingConfig(data, tomlPath, log)
	if err := hydrateAdapterInstructionFiles(&cfg, tomlPath); err != nil {
		log.Warn("config.load.instructions_failed", "concern", "config", "component", "config",
			"subcomponent", "load",
			"path", tomlPath,
			"format", "toml",
			"err", err,
		)
		return nil, fmt.Errorf("invalid %s: %w", tomlPath, err)
	}
	if err := applyLoggingDefaultsAndValidate(&cfg); err != nil {
		log.Warn("config.load.validate_failed", "concern", "config", "component", "config",
			"subcomponent", "load",
			"path", tomlPath,
			"format", "toml",
			"err", err,
		)
		return nil, fmt.Errorf("invalid %s: %w", tomlPath, err)
	}
	log.Debug("config.load.loaded", "concern", "config", "component", "config",
		"subcomponent", "load",
		"format", "toml",
		"path", tomlPath,
	)
	return &cfg, nil
}

type removedLoggingConfig struct {
	Logging removedLoggingSection `toml:"logging"`
	MITM    removedMITMSection    `toml:"mitm"`
	Adapter removedAdapterSection `toml:"adapter"`
}

// removedAdapterSection scans for the per-provider wire-capture blocks removed
// when the adapter wire-capture feature was dropped. go-toml/v2 ignores unknown
// keys, so a stale [adapter.<provider>.wire_capture] block would otherwise load
// silently; the scanner warns the operator instead.
type removedAdapterSection struct {
	Anthropic removedAdapterProvider `toml:"anthropic"`
	Codex     removedAdapterProvider `toml:"codex"`
}

type removedAdapterProvider struct {
	WireCapture *removedWireCapture `toml:"wire_capture"`
}

type removedWireCapture struct {
	Mode *string `toml:"mode"`
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

func warnRemovedLoggingConfig(data []byte, tomlPath string, log *slog.Logger) {
	removedConfig, err := unmarshalRemovedLoggingConfig(data)
	if err != nil {
		return
	}
	emitRemovedLoggingKeyWarning := func(key string) {
		log.Warn("config.load.removed_logging_key", "concern", "config", "component", "config",
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
	if removedConfig.Adapter.Anthropic.WireCapture != nil {
		emitRemovedLoggingKeyWarning("adapter.anthropic.wire_capture")
	}
	if removedConfig.Adapter.Codex.WireCapture != nil {
		emitRemovedLoggingKeyWarning("adapter.codex.wire_capture")
	}
}

func unmarshalRemovedLoggingConfig(data []byte) (removedLoggingConfig, error) {
	var removedConfig removedLoggingConfig
	if err := toml.Unmarshal(data, &removedConfig); err != nil {
		slog.Warn("config.load.removed_logging_config_scan_failed", "concern", "config", "component", "config",
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
	log := slog.Default()
	configDir := filepath.Dir(configPath)
	for name, model := range cfg.Adapter.Models {
		contents, err := loadInstructionFile(configDir, model.InstructionsFile)
		if err != nil {
			log.Warn("config.load.instructions_file_failed", "concern", "config", "component", "config",
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
	return nil
}

func loadInstructionFile(configDir string, configuredPath string) (string, error) {
	trimmedPath := strings.TrimSpace(configuredPath)
	if trimmedPath == "" {
		return "", nil
	}
	resolvedPath := cleanExpandedPath(trimmedPath)
	if !filepath.IsAbs(resolvedPath) {
		resolvedPath = filepath.Join(configDir, resolvedPath)
	}
	contents, err := os.ReadFile(resolvedPath)
	if err != nil {
		slog.Warn("config.load.instructions_file_read_failed",
			"concern", "process.daemon.config",
			"component", "config",
			"subcomponent", "load",
			"path", resolvedPath,
			"err", err,
		)
		return "", fmt.Errorf("read instructions file %s: %w", resolvedPath, err)
	}
	if len(contents) == 0 {
		err := fmt.Errorf("read %q: file is empty", resolvedPath)
		slog.Default().Warn("config.load.instructions_file_empty", "concern", "config", "component", "config",
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

// NewConfigWithDefaults is part of Clyde's typed adapter surface.
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

func applyLoggingDefaultsAndValidate(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	if err := applyDaemonDefaultsAndValidate(&cfg.Daemon); err != nil {
		return err
	}
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

	if err := applyMITMDefaultsAndValidate(&cfg.MITM); err != nil {
		return err
	}

	applyConversationDefaults(&cfg.Conversation)

	cfg.Adapter.Codex.ReasoningSummary = normalizeCodexReasoningSummary(cfg.Adapter.Codex.ReasoningSummary)
	switch codexReasoningSummary(cfg.Adapter.Codex.ReasoningSummary) {
	case codexReasoningSummaryAuto, codexReasoningSummaryConcise, codexReasoningSummaryDetailed, codexReasoningSummaryNone:
	default:
		return fmt.Errorf("adapter.codex.reasoning_summary must be one of auto|concise|detailed|none")
	}

	if err := applyAdapterNoticeDefaultsAndValidate(&cfg.Adapter.Notices); err != nil {
		return err
	}
	if err := applyAdapterRetryDefaultsAndValidate(&cfg.Adapter.Retry); err != nil {
		return err
	}

	if err := applyAdapterReasoningDefaultsAndValidate(&cfg.Adapter); err != nil {
		return err
	}
	return validateAdapterModelCatalog(&cfg.Adapter)
}

func applyLoggingCoreDefaults(logging *LoggingConfig) error {
	logLevel := strings.ToLower(strings.TrimSpace(logging.Level))
	if logLevel == "" {
		logLevel = "info"
	}
	logging.Level = logLevel
	logging.Paths.CLI = cleanExpandedPath(strings.TrimSpace(logging.Paths.CLI))
	logging.Paths.Daemon = cleanExpandedPath(strings.TrimSpace(logging.Paths.Daemon))
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
		if !IsKnownLoggingSink(normalizedName) {
			return fmt.Errorf("logging.sinks.enabled contains unknown sink %q", sinkName)
		}
		if seen[normalizedName] {
			continue
		}
		seen[normalizedName] = true
		normalizedEnabled = append(normalizedEnabled, normalizedName)
	}
	sinks.Enabled = normalizedEnabled
	return normalizeLoggingSinkOverrides(sinks)
}

// normalizeLoggingSinkOverrides validates and normalizes each per-sink config
// table that an operator may set with `[logging.sinks.<name>]`. It iterates the
// canonical sink registry so a sink table cannot be validated unless the sink
// is known, and writes the normalized override back to the matching struct
// field.
func normalizeLoggingSinkOverrides(sinks *LoggingSinks) error {
	for _, spec := range LoggingSinkSpecs() {
		override, ok := sinks.Override(spec.Name)
		if !ok {
			continue
		}
		path := "logging.sinks." + spec.Name
		if err := validateLoggingLevel(path+".level", override.Level); err != nil {
			return err
		}
		if err := validateLoggingRotation(path+".rotation", override.Rotation); err != nil {
			return err
		}
		override.Level = normalizeLoggingOptionalValue(override.Level)
		override.Path = strings.TrimSpace(override.Path)
		if err := sinks.setOverride(spec.Name, override); err != nil {
			return err
		}
	}
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
	switch loggingIncompletePolicy(policy) {
	case loggingIncompletePolicyWarn, loggingIncompletePolicyFailTest:
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
	switch loggingInventoryMode(mode) {
	case loggingInventoryModeIndexed, loggingInventoryModeDeep:
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
			if !IsKnownLoggingSink(sinkName) {
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
	switch loggingDetail(normalizeLoggingOptionalValue(detail)) {
	case loggingDetailUnset, loggingDetailSummary, loggingDetailVerbose:
		return nil
	default:
		return fmt.Errorf("%s.detail must be one of summary|verbose", path)
	}
}

func validateLoggingLevel(path string, level string) error {
	switch loggingLevel(strings.ToLower(strings.TrimSpace(level))) {
	case loggingLevelUnset, loggingLevelDebug, loggingLevelInfo, loggingLevelWarn, loggingLevelError:
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
	captureRules, err := normalizeMITMCaptureRouteRules(mitm.CaptureRules)
	if err != nil {
		return err
	}
	mitm.CaptureRules = captureRules
	if err := applyMITMListenerDefaultsAndValidate(mitm); err != nil {
		return err
	}
	applyMITMCaptureStoreDefaults(&mitm.CaptureStore, mitm.CaptureDir)
	if err := applyMITMCADefaultsAndValidate(&mitm.CA); err != nil {
		return err
	}
	return nil
}

func normalizeMITMCaptureDir(captureDir string) string {
	if captureDir == "" {
		return ""
	}
	cleanCaptureDir := cleanExpandedPath(captureDir)
	if filepath.Base(cleanCaptureDir) == "always-on" {
		return filepath.Dir(cleanCaptureDir)
	}
	return cleanCaptureDir
}

const (
	defaultMITMListenHost = "[::1]"
	defaultMITMListenPort = 48723
	defaultMITMListenerID = "default"
)

func normalizeMITMListener(id string, listener *MITMListenerConfig) error {
	listener.ID = strings.TrimSpace(id)
	if listener.ID == "" {
		return fmt.Errorf("mitm listener has an empty id; listener names are required")
	}
	listener.Host = strings.TrimSpace(listener.Host)
	if listener.Host == "" {
		listener.Host = defaultMITMListenHost
	}
	if listener.Port == 0 {
		listener.Port = defaultMITMListenPort
		return nil
	}
	if listener.Port < 0 || listener.Port > 65535 {
		return fmt.Errorf("mitm.%s.port %d is outside the valid TCP port range 1-65535", listener.ID, listener.Port)
	}
	return nil
}

// applyMITMCaptureStoreDefaults defaults the capture store DBPath to
// <captureDir>/capture.db when unset. The remaining store fields default
// inside the capture package when zero, so they are left untouched here.
func applyMITMCaptureStoreDefaults(store *MITMCaptureStoreConfig, captureDir string) {
	store.DBPath = strings.TrimSpace(store.DBPath)
	if store.DBPath == "" {
		store.DBPath = filepath.Join(captureDir, "capture.db")
		return
	}
	store.DBPath = cleanExpandedPath(store.DBPath)
}

func applyMITMCADefaultsAndValidate(ca *MITMCAConfig) error {
	caDir := filepath.Join(DefaultStateDir(), "mitm", "ca")

	ca.CertPath = strings.TrimSpace(ca.CertPath)
	if ca.CertPath == "" {
		ca.CertPath = filepath.Join(caDir, "clyde-mitm-ca.crt")
	} else {
		ca.CertPath = cleanExpandedPath(ca.CertPath)
	}
	if !filepath.IsAbs(ca.CertPath) {
		return fmt.Errorf("mitm.ca.cert_path %q must be an absolute path after expansion", ca.CertPath)
	}

	ca.KeyPath = strings.TrimSpace(ca.KeyPath)
	if ca.KeyPath == "" {
		ca.KeyPath = filepath.Join(caDir, "clyde-mitm-ca.key")
	} else {
		ca.KeyPath = cleanExpandedPath(ca.KeyPath)
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
			Method:              MITMMethodPost,
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
	rule.Provider = MITMProvider(normalizeMITMProviderName(string(rule.Provider)))
	if !rule.Provider.IsValid() {
		return MITMCaptureRouteRule{}, fmt.Errorf("provider %q is invalid", rule.Provider)
	}
	rule.Host = strings.Trim(strings.ToLower(strings.TrimSpace(rule.Host)), ".")
	rule.Method = MITMHTTPMethod(strings.ToUpper(strings.TrimSpace(string(rule.Method))))
	if !rule.Method.IsValid() {
		return MITMCaptureRouteRule{}, fmt.Errorf("method %q is invalid", rule.Method)
	}
	rule.PathExact = strings.TrimSpace(rule.PathExact)
	rule.PathPrefix = strings.TrimSpace(rule.PathPrefix)
	rule.PathContains = strings.TrimSpace(rule.PathContains)
	rule.ContentTypeContains = strings.ToLower(strings.TrimSpace(rule.ContentTypeContains))
	return rule, nil
}

func applyAdapterReasoningDefaultsAndValidate(adapter *AdapterConfig) error {
	if adapter == nil {
		return nil
	}
	if err := adapter.Anthropic.Reasoning.InboundThinking.Validate(); err != nil {
		return fmt.Errorf("adapter.anthropic.reasoning.inbound_thinking must be one of native_thinking_block|drop|plain_text_concat|passthrough (got %q)", adapter.Anthropic.Reasoning.InboundThinking)
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
	switch codexReasoningSummary(strings.ToLower(strings.TrimSpace(v))) {
	case "", codexReasoningSummaryAuto:
		return string(codexReasoningSummaryAuto)
	case codexReasoningSummaryConcise:
		return string(codexReasoningSummaryConcise)
	case codexReasoningSummaryDetailed:
		return string(codexReasoningSummaryDetailed)
	case codexReasoningSummaryNone:
		return string(codexReasoningSummaryNone)
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
	out := AdapterNoticeRepeatPolicy{Mode: mode, Cooldown: "", CooldownDuration: 0, CooldownTurns: 0}
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
