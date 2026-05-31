// Package logpolicy resolves Clyde logging config into narrow runtime policies.
package logpolicy

import (
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

// SinkName identifies a stable logging destination.
type SinkName string

const (
	// SinkDaemon identifies the daemon process log sink.
	SinkDaemon SinkName = config.LoggingSinkDaemon
	// SinkCLI identifies the CLI process log sink.
	SinkCLI SinkName = config.LoggingSinkCLI
	// SinkCodexSidecar identifies the Codex sidecar log sink.
	SinkCodexSidecar SinkName = config.LoggingSinkCodexSidecar
	// SinkAnthropicSidecar identifies the Anthropic sidecar log sink.
	SinkAnthropicSidecar SinkName = config.LoggingSinkAnthropicSidecar
	// SinkAudit identifies the cross-process audit log sink.
	SinkAudit SinkName = config.LoggingSinkAudit
	// SinkConcerns identifies the structured concern log sink.
	SinkConcerns SinkName = config.LoggingSinkConcerns
	// SinkTranscripts identifies the per-chat transcript log sink.
	SinkTranscripts SinkName = config.LoggingSinkTranscripts
	// SinkInventory identifies the inventory index sink.
	SinkInventory SinkName = config.LoggingSinkInventory
)

// Level is the minimum slog level accepted by a policy.
type Level string

const (
	// LevelDebug emits debug and above.
	LevelDebug Level = "debug"
	// LevelInfo emits info and above.
	LevelInfo Level = "info"
	// LevelWarn emits warnings and errors.
	LevelWarn Level = "warn"
	// LevelError emits errors only.
	LevelError Level = "error"
)

// Detail controls how much non-payload metadata a sink or concern may emit.
type Detail string

const (
	// DetailSummary emits summary metadata only.
	DetailSummary Detail = "summary"
	// DetailVerbose emits expanded metadata without raw payloads.
	DetailVerbose Detail = "verbose"
)

// PolicySet is an immutable snapshot of resolved logging controls.
type PolicySet struct {
	Sinks    map[SinkName]SinkPolicy
	Concerns map[string]ConcernPolicy
}

// SinkPolicy is the resolved policy for a named logging sink.
type SinkPolicy struct {
	Name     SinkName
	Enabled  bool
	Level    Level
	Detail   Detail
	Rotation RotationPolicy
	Cleanup  CleanupPolicy
}

// ConcernPolicy is the resolved policy for a registered concern.
type ConcernPolicy struct {
	Name     string
	Sink     SinkName
	Enabled  bool
	Level    Level
	Detail   Detail
	Rotation RotationPolicy
	Cleanup  CleanupPolicy
}

// RotationPolicy is the resolved file-rotation budget.
type RotationPolicy struct {
	Enabled    bool
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
}

// CleanupPolicy is the resolved deletion budget for old log files.
type CleanupPolicy struct {
	Enabled    bool
	MaxAgeDays *int
	MaxBackups *int
	MaxTotalMB *int
}

// Resolve converts validated config into typed logging policies.
func Resolve(cfg config.Config) (PolicySet, error) {
	globalLevel, err := parseLevel("logging.level", cfg.Logging.Level, LevelInfo)
	if err != nil {
		return PolicySet{}, err
	}
	globalRotation := resolveRotation(cfg.Logging.Rotation, RotationPolicy{
		Enabled:    true,
		MaxSizeMB:  64,
		MaxBackups: 192,
		MaxAgeDays: 14,
		Compress:   true,
	})
	globalCleanup, err := resolveCleanup("logging.cleanup", cfg.Logging.Cleanup, CleanupPolicy{
		Enabled:    true,
		MaxAgeDays: intPointer(14),
		MaxBackups: intPointer(192),
		MaxTotalMB: intPointer(4096),
	})
	if err != nil {
		return PolicySet{}, err
	}

	sinkPolicies := resolveSinks(cfg, globalLevel, globalRotation, globalCleanup)
	concernPolicies, err := resolveConcerns(cfg, sinkPolicies)
	if err != nil {
		return PolicySet{}, err
	}
	return PolicySet{
		Sinks:    cloneSinkPolicies(sinkPolicies),
		Concerns: cloneConcernPolicies(concernPolicies),
	}, nil
}

func resolveSinks(
	cfg config.Config,
	globalLevel Level,
	globalRotation RotationPolicy,
	globalCleanup CleanupPolicy,
) map[SinkName]SinkPolicy {
	enabledOverride := enabledSinkOverride(cfg.Logging.Sinks.Enabled)
	specs := config.LoggingSinkSpecs()
	policies := make(map[SinkName]SinkPolicy, len(specs))
	for _, spec := range specs {
		sinkName := SinkName(spec.Name)
		level := globalLevel
		rotation := globalRotation
		enabled := defaultSinkEnabled(cfg, spec, enabledOverride)
		if override, ok := cfg.Logging.Sinks.Override(spec.Name); ok {
			level = sinkOverrideLevel("logging.sinks."+spec.Name, override.Level, level)
			rotation = resolveRotation(override.Rotation, rotation)
			if override.Enabled != nil {
				enabled = *override.Enabled
			}
		}
		policies[sinkName] = SinkPolicy{
			Name:     sinkName,
			Enabled:  enabled,
			Level:    level,
			Detail:   DetailSummary,
			Rotation: rotation,
			Cleanup:  globalCleanup,
		}
	}
	return policies
}

// sinkOverrideLevel parses a per-sink level override, falling back to the
// inherited level when the override is empty. Config validation already
// rejected malformed levels, so a parse failure here keeps the inherited level
// rather than surfacing a second error.
func sinkOverrideLevel(path string, value string, inherited Level) Level {
	level, err := parseLevel(path+".level", value, inherited)
	if err != nil {
		return inherited
	}
	return level
}

func enabledSinkOverride(enabled []string) map[SinkName]bool {
	if len(enabled) == 0 {
		return nil
	}
	override := make(map[SinkName]bool, len(enabled))
	for _, sinkName := range enabled {
		override[SinkName(strings.ToLower(strings.TrimSpace(sinkName)))] = true
	}
	return override
}

func resolveConcerns(
	cfg config.Config,
	sinkPolicies map[SinkName]SinkPolicy,
) (map[string]ConcernPolicy, error) {
	// Concerns are dynamic: the only concern files that need a resolved policy
	// are those the config overrides explicitly plus the adapter wire-capture
	// concerns that carry built-in rotation defaults. Every other concern uses
	// the SinkConcerns sink defaults that slogger's router applies directly.
	policies := make(map[string]ConcernPolicy, len(cfg.Logging.Concerns))
	applyAdapterWireCaptureConcernDefaults(cfg, sinkPolicies, policies)
	for concernName, concernConfig := range cfg.Logging.Concerns {
		policy, ok := policies[concernName]
		if !ok {
			policy = baseConcernPolicy(concernName, sinkPolicies[SinkConcerns])
		}
		if concernConfig.Sink != "" {
			sinkName := SinkName(strings.ToLower(strings.TrimSpace(concernConfig.Sink)))
			sinkPolicy, ok := sinkPolicies[sinkName]
			if !ok {
				return nil, fmt.Errorf("logging.concerns.%s.sink is not a known sink", concernName)
			}
			policy.Sink = sinkName
			policy.Enabled = sinkPolicy.Enabled
			policy.Level = sinkPolicy.Level
			policy.Detail = sinkPolicy.Detail
			policy.Rotation = sinkPolicy.Rotation
			policy.Cleanup = sinkPolicy.Cleanup
		}
		if concernConfig.Enabled != nil {
			policy.Enabled = *concernConfig.Enabled
		}
		level, err := parseLevel("logging.concerns."+concernName+".level", concernConfig.Level, policy.Level)
		if err != nil {
			return nil, err
		}
		detail, err := parseDetail("logging.concerns."+concernName+".detail", concernConfig.Detail, policy.Detail)
		if err != nil {
			return nil, err
		}
		policy.Level = level
		policy.Detail = detail
		policy.Rotation = resolveRotation(concernConfig.Rotation, policy.Rotation)
		policies[concernName] = policy
	}
	return policies, nil
}

// baseConcernPolicy returns the concern policy a concern inherits from the
// structured concern sink before any per-concern config override applies.
func baseConcernPolicy(concernName string, sinkPolicy SinkPolicy) ConcernPolicy {
	return ConcernPolicy{
		Name:     concernName,
		Sink:     SinkConcerns,
		Enabled:  sinkPolicy.Enabled,
		Level:    sinkPolicy.Level,
		Detail:   sinkPolicy.Detail,
		Rotation: sinkPolicy.Rotation,
		Cleanup:  sinkPolicy.Cleanup,
	}
}

func applyAdapterWireCaptureConcernDefaults(
	cfg config.Config,
	sinkPolicies map[SinkName]SinkPolicy,
	policies map[string]ConcernPolicy,
) {
	rot := cfg.Adapter.WireCapture.Rotation
	if !loggingRotationSet(rot) {
		return
	}
	policy := RotationPolicy{
		Enabled:    true,
		MaxSizeMB:  8,
		MaxBackups: 3,
		MaxAgeDays: 2,
		Compress:   true,
	}
	policy = resolveRotation(rot, policy)
	for _, concernName := range []string{
		slogger.ConcernAdapterProviderAnthWire,
		slogger.ConcernAdapterProviderCodexWire,
	} {
		concernPolicy, ok := policies[concernName]
		if !ok {
			concernPolicy = baseConcernPolicy(concernName, sinkPolicies[SinkConcerns])
		}
		concernPolicy.Rotation = policy
		policies[concernName] = concernPolicy
	}
}

func loggingRotationSet(rot config.LoggingRotation) bool {
	return rot.Enabled != nil ||
		rot.MaxSizeMB != 0 ||
		rot.MaxBackups != 0 ||
		rot.MaxAgeDays != 0 ||
		rot.Compress != nil
}

func resolveRotation(cfg config.LoggingRotation, inherited RotationPolicy) RotationPolicy {
	policy := inherited
	if cfg.Enabled != nil {
		policy.Enabled = *cfg.Enabled
	}
	if cfg.MaxSizeMB > 0 {
		policy.MaxSizeMB = cfg.MaxSizeMB
	}
	if cfg.MaxBackups > 0 {
		policy.MaxBackups = cfg.MaxBackups
	}
	if cfg.MaxAgeDays > 0 {
		policy.MaxAgeDays = cfg.MaxAgeDays
	}
	if cfg.Compress != nil {
		policy.Compress = *cfg.Compress
	}
	return policy
}

func resolveCleanup(path string, cfg config.LoggingCleanup, inherited CleanupPolicy) (CleanupPolicy, error) {
	policy := inherited
	if cfg.Enabled != nil {
		policy.Enabled = *cfg.Enabled
	}
	if cfg.MaxAgeDays != nil {
		if *cfg.MaxAgeDays < 0 {
			return CleanupPolicy{}, fmt.Errorf("%s.max_age_days must be >= 0", path)
		}
		policy.MaxAgeDays = cloneIntPointer(cfg.MaxAgeDays)
	}
	if cfg.MaxBackups != nil {
		if *cfg.MaxBackups < 0 {
			return CleanupPolicy{}, fmt.Errorf("%s.max_backups must be >= 0", path)
		}
		policy.MaxBackups = cloneIntPointer(cfg.MaxBackups)
	}
	if cfg.MaxTotalMB != nil {
		if *cfg.MaxTotalMB < 0 {
			return CleanupPolicy{}, fmt.Errorf("%s.max_total_mb must be >= 0", path)
		}
		policy.MaxTotalMB = cloneIntPointer(cfg.MaxTotalMB)
	}
	return policy, nil
}

func parseLevel(path string, value string, inherited Level) (Level, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return inherited, nil
	}
	switch Level(normalized) {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
		return Level(normalized), nil
	default:
		return "", fmt.Errorf("%s must be one of debug|info|warn|error", path)
	}
}

func parseDetail(path string, value string, inherited Detail) (Detail, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return inherited, nil
	}
	switch Detail(normalized) {
	case DetailSummary, DetailVerbose:
		return Detail(normalized), nil
	default:
		return "", fmt.Errorf("%s must be one of summary|verbose", path)
	}
}

// defaultSinkEnabled resolves a sink's default-enabled state from its registry
// spec. The registry tags each sink with a [config.LoggingSinkDefaultRule]; this
// resolver applies the config-dependent gate the rule names so the per-sink
// roster lives in the registry rather than in a hand-maintained switch here.
func defaultSinkEnabled(cfg config.Config, spec config.LoggingSinkSpec, enabledOverride map[SinkName]bool) bool {
	baseEnabled := true
	if enabledOverride != nil {
		baseEnabled = enabledOverride[SinkName(spec.Name)]
	}
	switch spec.DefaultRule {
	case config.LoggingSinkDefaultTranscript:
		return baseEnabled && cfg.Logging.Transcript.IsEnabled()
	case config.LoggingSinkDefaultAlwaysOn:
		return baseEnabled
	default:
		return baseEnabled
	}
}

func cloneSinkPolicies(policies map[SinkName]SinkPolicy) map[SinkName]SinkPolicy {
	clonedPolicies := make(map[SinkName]SinkPolicy, len(policies))
	for sinkName, policy := range policies {
		policy.Cleanup = cloneCleanupPolicy(policy.Cleanup)
		clonedPolicies[sinkName] = policy
	}
	return clonedPolicies
}

func cloneConcernPolicies(policies map[string]ConcernPolicy) map[string]ConcernPolicy {
	clonedPolicies := make(map[string]ConcernPolicy, len(policies))
	for concernName, policy := range policies {
		policy.Cleanup = cloneCleanupPolicy(policy.Cleanup)
		clonedPolicies[concernName] = policy
	}
	return clonedPolicies
}

func cloneCleanupPolicy(policy CleanupPolicy) CleanupPolicy {
	return CleanupPolicy{
		Enabled:    policy.Enabled,
		MaxAgeDays: cloneIntPointer(policy.MaxAgeDays),
		MaxBackups: cloneIntPointer(policy.MaxBackups),
		MaxTotalMB: cloneIntPointer(policy.MaxTotalMB),
	}
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	clonedValue := *value
	return &clonedValue
}

// ResolveSloggerSetup converts config into the narrow setup contract consumed
// by slogger. The logging policy resolver owns config defaults and validation;
// slogger owns only handler construction and file lifecycle.
func ResolveSloggerSetup(cfg config.Config, role slogger.ProcessRole) (slogger.SetupPolicy, error) {
	policies, err := Resolve(cfg)
	if err != nil {
		return slogger.SetupPolicy{}, err
	}
	processSink := policies.Sinks[SinkCLI]
	if role == slogger.ProcessRoleDaemon {
		processSink = policies.Sinks[SinkDaemon]
	}
	concernSink := policies.Sinks[SinkConcerns]
	setupPolicy := slogger.SetupPolicy{
		Level: levelToSlog(processSink.Level),
		ProcessSink: slogger.FileSinkPolicy{
			Enabled: processSink.Enabled,
			Path:    slogger.DefaultProcessPath(cfg.Logging, role),
			Rotation: slogger.RotationPolicy{
				Enabled:    processSink.Rotation.Enabled,
				MaxSizeMB:  processSink.Rotation.MaxSizeMB,
				MaxBackups: processSink.Rotation.MaxBackups,
				MaxAgeDays: processSink.Rotation.MaxAgeDays,
				Compress:   boolPointer(processSink.Rotation.Compress),
			},
		},
		ConcernRoot:     slogger.DefaultConcernRoot(cfg.Logging, role),
		ConcernPolicies: resolveSloggerConcernPolicies(policies),
		TranscriptPolicy: slogger.TranscriptPolicy{
			Enabled: policies.Sinks[SinkTranscripts].Enabled && cfg.Logging.Transcript.IsEnabled(),
		},
		InventoryPolicy: slogger.InventoryPolicy{
			Enabled: policies.Sinks[SinkInventory].Enabled,
			Root:    "",
			Rotation: slogger.RotationPolicy{
				Enabled:    policies.Sinks[SinkInventory].Rotation.Enabled,
				MaxSizeMB:  policies.Sinks[SinkInventory].Rotation.MaxSizeMB,
				MaxBackups: policies.Sinks[SinkInventory].Rotation.MaxBackups,
				MaxAgeDays: policies.Sinks[SinkInventory].Rotation.MaxAgeDays,
				Compress:   boolPointer(policies.Sinks[SinkInventory].Rotation.Compress),
			},
		},
		CleanupPolicy: slogger.CleanupPolicy{
			// Cleanup is daemon-only. The walker scans the full state tree
			// (logs, MITM raw captures, etc.) and must never block a CLI process
			// start. The daemon runs RunCleanupOnce on a periodic loop instead.
			Enabled:    processSink.Cleanup.Enabled && role == slogger.ProcessRoleDaemon,
			Root:       config.DefaultStateDir(),
			MaxAgeDays: cleanupInt(processSink.Cleanup.MaxAgeDays),
			MaxBackups: cleanupInt(processSink.Cleanup.MaxBackups),
			MaxTotalMB: cleanupInt(processSink.Cleanup.MaxTotalMB),
		},
	}
	if !concernSink.Enabled {
		for concernName, concernPolicy := range setupPolicy.ConcernPolicies {
			concernPolicy.Enabled = boolPointer(false)
			setupPolicy.ConcernPolicies[concernName] = concernPolicy
		}
	}
	return setupPolicy, nil
}

func resolveSloggerConcernPolicies(policies PolicySet) map[string]slogger.ConcernPolicy {
	concernPolicies := make(map[string]slogger.ConcernPolicy, len(policies.Concerns))
	for concernName, concernPolicy := range policies.Concerns {
		sinkPolicy := policies.Sinks[concernPolicy.Sink]
		enabled := concernPolicy.Enabled && sinkPolicy.Enabled
		if concernPolicy.Sink != SinkConcerns {
			enabled = false
		}
		rotationPolicy := slogger.RotationPolicy{
			Enabled:    concernPolicy.Rotation.Enabled,
			MaxSizeMB:  concernPolicy.Rotation.MaxSizeMB,
			MaxBackups: concernPolicy.Rotation.MaxBackups,
			MaxAgeDays: concernPolicy.Rotation.MaxAgeDays,
			Compress:   boolPointer(concernPolicy.Rotation.Compress),
		}
		level := levelToSlog(concernPolicy.Level)
		concernPolicies[concernName] = slogger.ConcernPolicy{
			Enabled:  boolPointer(enabled),
			Level:    slogLevelPointer(level),
			Rotation: &rotationPolicy,
		}
	}
	return concernPolicies
}

func levelToSlog(level Level) slog.Level {
	switch level {
	case LevelDebug:
		return slog.LevelDebug
	case LevelInfo:
		return slog.LevelInfo
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func boolPointer(value bool) *bool {
	copiedValue := value
	return &copiedValue
}

func intPointer(value int) *int {
	copiedValue := value
	return &copiedValue
}

func slogLevelPointer(value slog.Level) *slog.Level {
	copiedValue := value
	return &copiedValue
}

func cleanupInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
