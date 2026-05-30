package config

import "fmt"

// LoggingConfig carries global logging settings.
type LoggingConfig struct {
	Level      string            `json:"level,omitempty" toml:"level,omitempty"`
	Rotation   LoggingRotation   `json:"rotation,omitzero" toml:"rotation,omitempty"`
	RawCapture LoggingToggle     `json:"raw_capture,omitzero" toml:"raw_capture,omitempty"`
	Cleanup    LoggingCleanup    `json:"cleanup,omitzero" toml:"cleanup,omitempty"`
	Request    LoggingRequest    `json:"request,omitzero" toml:"request,omitempty"`
	Inventory  LoggingInventory  `json:"inventory,omitzero" toml:"inventory,omitempty"`
	Paths      LoggingPaths      `json:"paths,omitzero" toml:"paths,omitempty"`
	Transcript LoggingTranscript `json:"transcript,omitzero" toml:"transcript,omitempty"`
	Sinks      LoggingSinks      `json:"sinks,omitzero" toml:"sinks,omitempty"`
	Concerns   LoggingConcerns   `json:"concerns,omitempty" toml:"concerns,omitempty"`
}

// LoggingSinks carries per-sink logging controls. The flat Enabled list is the
// back-compatible form (`[logging.sinks] enabled = ["daemon", ...]`); the
// per-sink fields are the per-sink-table form
// (`[logging.sinks.<name>] enabled = true`). Both forms feed the same resolved
// sink roster, so an operator may migrate one sink at a time. The per-sink
// fields are named after the canonical sink registry entries; resolve one by
// canonical name with [LoggingSinks.Override].
type LoggingSinks struct {
	Enabled          []string            `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Daemon           LoggingSinkOverride `json:"daemon,omitzero" toml:"daemon,omitempty"`
	CLI              LoggingSinkOverride `json:"cli,omitzero" toml:"cli,omitempty"`
	CodexSidecar     LoggingSinkOverride `json:"codex_sidecar,omitzero" toml:"codex_sidecar,omitempty"`
	AnthropicSidecar LoggingSinkOverride `json:"anthropic_sidecar,omitzero" toml:"anthropic_sidecar,omitempty"`
	Audit            LoggingSinkOverride `json:"audit,omitzero" toml:"audit,omitempty"`
	Concerns         LoggingSinkOverride `json:"concerns,omitzero" toml:"concerns,omitempty"`
	Transcripts      LoggingSinkOverride `json:"transcripts,omitzero" toml:"transcripts,omitempty"`
	MITMRaw          LoggingSinkOverride `json:"mitm_raw,omitzero" toml:"mitm_raw,omitempty"`
	Inventory        LoggingSinkOverride `json:"inventory_index,omitzero" toml:"inventory_index,omitempty"`
}

// Override returns the per-sink config table for a canonical sink name. The
// lookup is keyed off the canonical registry names so callers never branch on a
// struct field; an unknown name returns the zero override and ok=false.
func (s *LoggingSinks) Override(name string) (LoggingSinkOverride, bool) {
	switch name {
	case LoggingSinkDaemon:
		return s.Daemon, true
	case LoggingSinkCLI:
		return s.CLI, true
	case LoggingSinkCodexSidecar:
		return s.CodexSidecar, true
	case LoggingSinkAnthropicSidecar:
		return s.AnthropicSidecar, true
	case LoggingSinkAudit:
		return s.Audit, true
	case LoggingSinkConcerns:
		return s.Concerns, true
	case LoggingSinkTranscripts:
		return s.Transcripts, true
	case LoggingSinkMITMRaw:
		return s.MITMRaw, true
	case LoggingSinkInventory:
		return s.Inventory, true
	default:
		return LoggingSinkOverride{
			Enabled: nil,
			Level:   "",
			Path:    "",
			Rotation: LoggingRotation{
				Enabled:    nil,
				MaxSizeMB:  0,
				MaxBackups: 0,
				MaxAgeDays: 0,
				Compress:   nil,
			},
		}, false
	}
}

// setOverride writes a normalized per-sink config table back to the matching
// canonical struct field. An unknown name is rejected so a normalize pass can
// never silently drop a sink override.
func (s *LoggingSinks) setOverride(name string, override LoggingSinkOverride) error {
	switch name {
	case LoggingSinkDaemon:
		s.Daemon = override
	case LoggingSinkCLI:
		s.CLI = override
	case LoggingSinkCodexSidecar:
		s.CodexSidecar = override
	case LoggingSinkAnthropicSidecar:
		s.AnthropicSidecar = override
	case LoggingSinkAudit:
		s.Audit = override
	case LoggingSinkConcerns:
		s.Concerns = override
	case LoggingSinkTranscripts:
		s.Transcripts = override
	case LoggingSinkMITMRaw:
		s.MITMRaw = override
	case LoggingSinkInventory:
		s.Inventory = override
	default:
		return fmt.Errorf("logging.sinks: unknown sink %q", name)
	}
	return nil
}

// LoggingSinkOverride carries the per-sink config table fields. Enabled is a
// tri-state pointer so an unset table leaves the sink at its registry default
// while an explicit value (true or false) overrides it.
type LoggingSinkOverride struct {
	Enabled  *bool           `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Level    string          `json:"level,omitempty" toml:"level,omitempty"`
	Path     string          `json:"path,omitempty" toml:"path,omitempty"`
	Rotation LoggingRotation `json:"rotation,omitzero" toml:"rotation,omitempty"`
}

// LoggingConcerns maps registered concern names to per-concern overrides.
type LoggingConcerns map[string]LoggingConcern

const (
	// LoggingSinkDaemon names the daemon process log sink.
	LoggingSinkDaemon = "daemon"
	// LoggingSinkCLI names the CLI process log sink.
	LoggingSinkCLI = "cli"
	// LoggingSinkCodexSidecar names the Codex sidecar log sink.
	LoggingSinkCodexSidecar = "codex_sidecar"
	// LoggingSinkAnthropicSidecar names the Anthropic sidecar log sink.
	LoggingSinkAnthropicSidecar = "anthropic_sidecar"
	// LoggingSinkAudit names the cross-process audit log sink.
	LoggingSinkAudit = "audit"
	// LoggingSinkConcerns names the structured concern log sink.
	LoggingSinkConcerns = "concerns"
	// LoggingSinkTranscripts names the per-chat transcript sink.
	LoggingSinkTranscripts = "transcripts"
	// LoggingSinkMITMRaw names the MITM raw payload sink.
	LoggingSinkMITMRaw = "mitm_raw"
	// LoggingSinkInventory names the inventory index sink.
	LoggingSinkInventory = "inventory_index"
)

// LoggingSinkDefaultRule names how a sink's default-enabled state resolves.
// The registry tags each sink with a rule; the logging policy resolver reads
// the rule to apply config-dependent gates without hard-coding a per-sink
// switch of its own.
type LoggingSinkDefaultRule int

const (
	// LoggingSinkDefaultAlwaysOn marks a sink enabled by default whenever it is
	// in the active sink set.
	LoggingSinkDefaultAlwaysOn LoggingSinkDefaultRule = iota
	// LoggingSinkDefaultTranscript marks a sink whose default-enabled state is
	// additionally gated by [LoggingTranscript.IsEnabled].
	LoggingSinkDefaultTranscript
	// LoggingSinkDefaultRawCapture marks a sink whose default-enabled state is
	// additionally gated by logging.raw_capture.enabled.
	LoggingSinkDefaultRawCapture
)

// LoggingSinkSpec is one declarative row in the canonical sink registry. The
// registry is the single source of truth for the logging sink roster: config
// validation, the logging policy resolver, and slogger all derive their sink
// names and default-enabled rules from these specs rather than from
// independently maintained const lists or maps.
type LoggingSinkSpec struct {
	Name        string
	DefaultRule LoggingSinkDefaultRule
}

// loggingSinkSpecs is the ordered canonical sink roster. Adding, removing, or
// retagging a sink happens here once; every other layer iterates this table.
var loggingSinkSpecs = []LoggingSinkSpec{
	{Name: LoggingSinkDaemon, DefaultRule: LoggingSinkDefaultAlwaysOn},
	{Name: LoggingSinkCLI, DefaultRule: LoggingSinkDefaultAlwaysOn},
	{Name: LoggingSinkCodexSidecar, DefaultRule: LoggingSinkDefaultAlwaysOn},
	{Name: LoggingSinkAnthropicSidecar, DefaultRule: LoggingSinkDefaultAlwaysOn},
	{Name: LoggingSinkAudit, DefaultRule: LoggingSinkDefaultAlwaysOn},
	{Name: LoggingSinkConcerns, DefaultRule: LoggingSinkDefaultAlwaysOn},
	{Name: LoggingSinkTranscripts, DefaultRule: LoggingSinkDefaultTranscript},
	{Name: LoggingSinkMITMRaw, DefaultRule: LoggingSinkDefaultRawCapture},
	{Name: LoggingSinkInventory, DefaultRule: LoggingSinkDefaultAlwaysOn},
}

// LoggingSinkSpecs returns the canonical sink registry in roster order. The
// returned slice is a copy, so callers may iterate or sort it freely without
// mutating the source table.
func LoggingSinkSpecs() []LoggingSinkSpec {
	specs := make([]LoggingSinkSpec, len(loggingSinkSpecs))
	copy(specs, loggingSinkSpecs)
	return specs
}

// IsKnownLoggingSink reports whether name matches a sink in the canonical
// registry. Config validation asks the registry this question instead of
// consulting a separately maintained map.
func IsKnownLoggingSink(name string) bool {
	for _, spec := range loggingSinkSpecs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

// LoggingConcern carries config-layer controls for a registered concern.
type LoggingConcern struct {
	Enabled  *bool           `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Level    string          `json:"level,omitempty" toml:"level,omitempty"`
	Detail   string          `json:"detail,omitempty" toml:"detail,omitempty"`
	Sink     string          `json:"sink,omitempty" toml:"sink,omitempty"`
	Rotation LoggingRotation `json:"rotation,omitzero" toml:"rotation,omitempty"`
}

// LoggingTranscript controls the per-chat transcript router that tees a
// curated allowlist of records to chats/<chat_key>.jsonl, one file per chat.
// Default: on. The operator turns it off by setting Enabled = false.
type LoggingTranscript struct {
	// Enabled toggles the per-chat router. Default true.
	Enabled *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
}

// IsEnabled reports whether the transcript router should be wired in.
// Defaults to true; operators can flip Enabled = false to disable.
func (t LoggingTranscript) IsEnabled() bool {
	if t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// LoggingRotation controls file rotation behavior for the unified clyde logger.
type LoggingRotation struct {
	Enabled    *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	MaxSizeMB  int   `json:"max_size_mb,omitempty" toml:"max_size_mb,omitempty"`
	MaxBackups int   `json:"max_backups,omitempty" toml:"max_backups,omitempty"`
	MaxAgeDays int   `json:"max_age_days,omitempty" toml:"max_age_days,omitempty"`
	Compress   *bool `json:"compress,omitempty" toml:"compress,omitempty"`
}

// SidecarRotationDefaults are the shared default rotation settings for the
// dedicated provider JSONL sidecars (anthropic.jsonl, codex.jsonl). Both
// sidecars retain the same volume of history, so the defaults live in one
// place and each sidecar normalizer clamps to them via
// [NormalizeSidecarRotation].
const (
	// SidecarRotationMaxSizeMB is the default rotate-at size for provider
	// JSONL sidecars when the configured size is non-positive.
	SidecarRotationMaxSizeMB = 64
	// SidecarRotationMaxBackups is the default retained-backup count for
	// provider JSONL sidecars when the configured count is non-positive.
	SidecarRotationMaxBackups = 192
	// SidecarRotationMaxAgeDays is the default retention age for provider
	// JSONL sidecars when the configured age is non-positive.
	SidecarRotationMaxAgeDays = 14
)

// SidecarRotation is the normalized rotation shape shared by the provider
// JSONL sidecars. Compress is a tri-state pointer so a nil value can resolve
// to the documented default (compress on) while an explicit false is honored.
type SidecarRotation struct {
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   *bool
}

// NormalizeSidecarRotation clamps each non-positive size field to the shared
// [SidecarRotationDefaults] and defaults a nil Compress to true. It is the
// single normalization helper for the anthropic and codex JSONL sidecars so
// the two providers cannot drift apart on retention policy.
func NormalizeSidecarRotation(in SidecarRotation) SidecarRotation {
	if in.MaxSizeMB <= 0 {
		in.MaxSizeMB = SidecarRotationMaxSizeMB
	}
	if in.MaxBackups <= 0 {
		in.MaxBackups = SidecarRotationMaxBackups
	}
	if in.MaxAgeDays <= 0 {
		in.MaxAgeDays = SidecarRotationMaxAgeDays
	}
	if in.Compress == nil {
		enabled := true
		in.Compress = &enabled
	}
	return in
}

// LoggingCleanup controls deletion of old log files separately from file rotation.
type LoggingCleanup struct {
	Enabled    *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	MaxAgeDays *int  `json:"max_age_days,omitempty" toml:"max_age_days,omitempty"`
	MaxBackups *int  `json:"max_backups,omitempty" toml:"max_backups,omitempty"`
	MaxTotalMB *int  `json:"max_total_mb,omitempty" toml:"max_total_mb,omitempty"`
}

// LoggingToggle is a named on/off logging control.
type LoggingToggle struct {
	Enabled *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
}

// LoggingRequest controls request-story contract checks.
type LoggingRequest struct {
	RequiredLegs     map[string][]string `json:"required_legs,omitempty" toml:"required_legs,omitempty"`
	IncompletePolicy string              `json:"incomplete_policy,omitempty" toml:"incomplete_policy,omitempty"`
}

// LoggingInventory controls how clyde logs inventory discovers log locations.
type LoggingInventory struct {
	Mode string `json:"mode,omitempty" toml:"mode,omitempty"`
}

// LoggingPaths controls per-process JSONL destinations. When a path is empty,
// slogger picks a process-specific default under $XDG_STATE_HOME/clyde.
type LoggingPaths struct {
	CLI    string `json:"cli,omitempty" toml:"cli,omitempty"`
	Daemon string `json:"daemon,omitempty" toml:"daemon,omitempty"`
}
