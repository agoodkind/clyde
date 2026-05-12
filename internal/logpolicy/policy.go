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
	// SinkTUI identifies the TUI process log sink.
	SinkTUI SinkName = config.LoggingSinkTUI
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
	// SinkMITMCapture identifies the MITM capture index sink.
	SinkMITMCapture SinkName = config.LoggingSinkMITMCapture
	// SinkMITMRaw identifies the MITM raw payload sink.
	SinkMITMRaw SinkName = config.LoggingSinkMITMRaw
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

// Detail controls how much payload detail a sink or concern may emit.
type Detail string

const (
	// DetailSummary emits summary metadata only.
	DetailSummary Detail = "summary"
	// DetailVerbose emits expanded metadata without raw payloads.
	DetailVerbose Detail = "verbose"
	// DetailRaw emits full local-debug payloads where the caller supports them.
	DetailRaw Detail = "raw"
	// DetailOff disables detail surfaces that honor detail policy.
	DetailOff Detail = "off"
)

// CleanupMode controls retention cleanup behavior.
type CleanupMode string

const (
	// CleanupOff disables cleanup.
	CleanupOff CleanupMode = "off"
	// CleanupAuditOnly reports cleanup candidates without deleting them.
	CleanupAuditOnly CleanupMode = "audit_only"
	// CleanupDelete deletes cleanup candidates selected by retention policy.
	CleanupDelete CleanupMode = "delete"
)

// PolicySet is an immutable snapshot of resolved logging controls.
type PolicySet struct {
	Sinks    map[SinkName]SinkPolicy
	Concerns map[string]ConcernPolicy
}

// SinkPolicy is the resolved policy for a named logging sink.
type SinkPolicy struct {
	Name      SinkName
	Enabled   bool
	Level     Level
	Detail    Detail
	Rotation  RotationPolicy
	Retention RetentionPolicy
}

// ConcernPolicy is the resolved policy for a registered concern.
type ConcernPolicy struct {
	Name      string
	Sink      SinkName
	Enabled   bool
	Level     Level
	Detail    Detail
	Rotation  RotationPolicy
	Retention RetentionPolicy
}

// RotationPolicy is the resolved file-rotation budget.
type RotationPolicy struct {
	Enabled    bool
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
}

// RetentionPolicy is the resolved cleanup budget.
type RetentionPolicy struct {
	MaxAgeDays  *int
	MaxBackups  *int
	MaxTotalMB  *int
	Compress    bool
	CleanupMode CleanupMode
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
	globalRetention, err := resolveRetention("logging.retention", cfg.Logging.Retention, RetentionPolicy{
		MaxAgeDays:  nil,
		MaxBackups:  nil,
		MaxTotalMB:  nil,
		Compress:    globalRotation.Compress,
		CleanupMode: CleanupOff,
	})
	if err != nil {
		return PolicySet{}, err
	}

	sinkPolicies, err := resolveSinks(cfg, globalLevel, globalRotation, globalRetention)
	if err != nil {
		return PolicySet{}, err
	}
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
	globalRetention RetentionPolicy,
) (map[SinkName]SinkPolicy, error) {
	policies := make(map[SinkName]SinkPolicy, len(allSinkNames()))
	for _, sinkName := range allSinkNames() {
		policies[sinkName] = SinkPolicy{
			Name:      sinkName,
			Enabled:   defaultSinkEnabled(cfg, sinkName),
			Level:     globalLevel,
			Detail:    DetailSummary,
			Rotation:  globalRotation,
			Retention: globalRetention,
		}
	}
	for rawSinkName, sinkConfig := range cfg.Logging.Sinks {
		sinkName := SinkName(strings.ToLower(strings.TrimSpace(rawSinkName)))
		policy, ok := policies[sinkName]
		if !ok {
			return nil, fmt.Errorf("logging.sinks.%s is not a known sink", rawSinkName)
		}
		if sinkConfig.Enabled != nil {
			policy.Enabled = *sinkConfig.Enabled
		}
		level, err := parseLevel("logging.sinks."+rawSinkName+".level", sinkConfig.Level, policy.Level)
		if err != nil {
			return nil, err
		}
		detail, err := parseDetail("logging.sinks."+rawSinkName+".detail", sinkConfig.Detail, policy.Detail)
		if err != nil {
			return nil, err
		}
		retention, err := resolveRetention(
			"logging.sinks."+rawSinkName+".retention",
			sinkConfig.Retention,
			policy.Retention,
		)
		if err != nil {
			return nil, err
		}
		policy.Level = level
		policy.Detail = detail
		policy.Rotation = resolveRotation(sinkConfig.Rotation, policy.Rotation)
		policy.Retention = retention
		policies[sinkName] = policy
	}
	return policies, nil
}

func resolveConcerns(
	cfg config.Config,
	sinkPolicies map[SinkName]SinkPolicy,
) (map[string]ConcernPolicy, error) {
	policies := make(map[string]ConcernPolicy, len(slogger.KnownConcerns()))
	for _, concernName := range slogger.KnownConcerns() {
		sinkPolicy := sinkPolicies[SinkConcerns]
		policies[concernName] = ConcernPolicy{
			Name:      concernName,
			Sink:      SinkConcerns,
			Enabled:   sinkPolicy.Enabled,
			Level:     sinkPolicy.Level,
			Detail:    sinkPolicy.Detail,
			Rotation:  sinkPolicy.Rotation,
			Retention: sinkPolicy.Retention,
		}
	}
	applyAdapterWireCaptureConcernDefaults(cfg, policies)
	for concernName, concernConfig := range cfg.Logging.Concerns {
		if !slogger.IsKnownConcern(concernName) {
			return nil, fmt.Errorf("logging.concerns.%s is not a known concern", concernName)
		}
		policy := policies[concernName]
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
			policy.Retention = sinkPolicy.Retention
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
		retention, err := resolveRetention(
			"logging.concerns."+concernName+".retention",
			concernConfig.Retention,
			policy.Retention,
		)
		if err != nil {
			return nil, err
		}
		policy.Level = level
		policy.Detail = detail
		policy.Rotation = resolveRotation(concernConfig.Rotation, policy.Rotation)
		policy.Retention = retention
		policies[concernName] = policy
	}
	return policies, nil
}

func applyAdapterWireCaptureConcernDefaults(cfg config.Config, policies map[string]ConcernPolicy) {
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
		concernPolicy := policies[concernName]
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

func resolveRetention(
	path string,
	cfg config.LoggingRetention,
	inherited RetentionPolicy,
) (RetentionPolicy, error) {
	policy := inherited
	if cfg.MaxAgeDays != nil {
		if *cfg.MaxAgeDays < 0 {
			return RetentionPolicy{}, fmt.Errorf("%s.max_age_days must be >= 0", path)
		}
		policy.MaxAgeDays = cloneIntPointer(cfg.MaxAgeDays)
	}
	if cfg.MaxBackups != nil {
		if *cfg.MaxBackups < 0 {
			return RetentionPolicy{}, fmt.Errorf("%s.max_backups must be >= 0", path)
		}
		policy.MaxBackups = cloneIntPointer(cfg.MaxBackups)
	}
	if cfg.MaxTotalMB != nil {
		if *cfg.MaxTotalMB < 0 {
			return RetentionPolicy{}, fmt.Errorf("%s.max_total_mb must be >= 0", path)
		}
		policy.MaxTotalMB = cloneIntPointer(cfg.MaxTotalMB)
	}
	if cfg.Compress != nil {
		policy.Compress = *cfg.Compress
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.CleanupMode))
	if mode == "" {
		return policy, nil
	}
	switch CleanupMode(mode) {
	case CleanupOff, CleanupAuditOnly, CleanupDelete:
		policy.CleanupMode = CleanupMode(mode)
		return policy, nil
	default:
		return RetentionPolicy{}, fmt.Errorf("%s.cleanup_mode must be one of off|audit_only|delete", path)
	}
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
	case DetailSummary, DetailVerbose, DetailRaw, DetailOff:
		return Detail(normalized), nil
	default:
		return "", fmt.Errorf("%s must be one of summary|verbose|raw|off", path)
	}
}

func defaultSinkEnabled(cfg config.Config, sinkName SinkName) bool {
	if sinkName == SinkTranscripts {
		return cfg.Logging.Transcript.IsEnabled()
	}
	return true
}

func allSinkNames() []SinkName {
	return []SinkName{
		SinkDaemon,
		SinkTUI,
		SinkCodexSidecar,
		SinkAnthropicSidecar,
		SinkAudit,
		SinkConcerns,
		SinkTranscripts,
		SinkMITMCapture,
		SinkMITMRaw,
	}
}

func cloneSinkPolicies(policies map[SinkName]SinkPolicy) map[SinkName]SinkPolicy {
	clonedPolicies := make(map[SinkName]SinkPolicy, len(policies))
	for sinkName, policy := range policies {
		policy.Retention = cloneRetentionPolicy(policy.Retention)
		clonedPolicies[sinkName] = policy
	}
	return clonedPolicies
}

func cloneConcernPolicies(policies map[string]ConcernPolicy) map[string]ConcernPolicy {
	clonedPolicies := make(map[string]ConcernPolicy, len(policies))
	for concernName, policy := range policies {
		policy.Retention = cloneRetentionPolicy(policy.Retention)
		clonedPolicies[concernName] = policy
	}
	return clonedPolicies
}

func cloneRetentionPolicy(policy RetentionPolicy) RetentionPolicy {
	return RetentionPolicy{
		MaxAgeDays:  cloneIntPointer(policy.MaxAgeDays),
		MaxBackups:  cloneIntPointer(policy.MaxBackups),
		MaxTotalMB:  cloneIntPointer(policy.MaxTotalMB),
		Compress:    policy.Compress,
		CleanupMode: policy.CleanupMode,
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
	processSink := policies.Sinks[SinkTUI]
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
				Compress:   new(processSink.Rotation.Compress),
			},
		},
		ConcernRoot:     slogger.DefaultConcernRoot(cfg.Logging, role),
		ConcernPolicies: resolveSloggerConcernPolicies(policies),
		TranscriptPolicy: slogger.TranscriptPolicy{
			Enabled: policies.Sinks[SinkTranscripts].Enabled && cfg.Logging.Transcript.IsEnabled(),
			Mode:    transcriptModeToSlogger(cfg.Logging.Transcript.Mode),
		},
	}
	if !concernSink.Enabled {
		for concernName, concernPolicy := range setupPolicy.ConcernPolicies {
			concernPolicy.Enabled = new(false)
			setupPolicy.ConcernPolicies[concernName] = concernPolicy
		}
	}
	return setupPolicy, nil
}

func resolveSloggerConcernPolicies(policies PolicySet) map[string]slogger.ConcernPolicy {
	concernPolicies := make(map[string]slogger.ConcernPolicy, len(policies.Concerns))
	for concernName, concernPolicy := range policies.Concerns {
		sinkPolicy := policies.Sinks[concernPolicy.Sink]
		enabled := concernPolicy.Enabled && sinkPolicy.Enabled && concernPolicy.Detail != DetailOff
		if concernPolicy.Sink != SinkConcerns {
			enabled = false
		}
		rotationPolicy := slogger.RotationPolicy{
			Enabled:    concernPolicy.Rotation.Enabled,
			MaxSizeMB:  concernPolicy.Rotation.MaxSizeMB,
			MaxBackups: concernPolicy.Rotation.MaxBackups,
			MaxAgeDays: concernPolicy.Rotation.MaxAgeDays,
			Compress:   new(concernPolicy.Rotation.Compress),
		}
		level := levelToSlog(concernPolicy.Level)
		concernPolicies[concernName] = slogger.ConcernPolicy{
			Enabled:  new(enabled),
			Level:    new(level),
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

func transcriptModeToSlogger(mode string) slogger.TranscriptMode {
	if mode == string(slogger.TranscriptModeRaw) {
		return slogger.TranscriptModeRaw
	}
	return slogger.TranscriptModeSummary
}
