package config

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

// LoggingSinks carries the enabled central sink names.
type LoggingSinks struct {
	Enabled []string `json:"enabled,omitempty" toml:"enabled,omitempty"`
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
	// LoggingSinkMITMCapture names the MITM capture index sink.
	LoggingSinkMITMCapture = "mitm_capture"
	// LoggingSinkMITMRaw names the MITM raw payload sink.
	LoggingSinkMITMRaw = "mitm_raw"
	// LoggingSinkInventory names the inventory index sink.
	LoggingSinkInventory = "inventory_index"
)

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
