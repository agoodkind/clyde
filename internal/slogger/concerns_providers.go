package slogger

// Provider concern constants. The set covers live providers code
// only. Subsystems whose code was deleted in the raw-transcript plus
// adapter refactor (Claude session lifecycle, discovery, settings,
// transcript, remote-control, cleanup; Codex session lifecycle,
// discovery, transcript, cleanup) no longer have constants here;
// removing them keeps the registry honest about which JSONL files
// can actually receive records.
const (
	// ConcernProviderClaudeOAuth covers slog records from the
	// surviving claude/oauthcredentials reader: macOS keychain
	// lookups and OAuth blob parse failures.
	ConcernProviderClaudeOAuth = "providers.claude.oauth"
	// ConcernProviderCodexStore covers slog records from the
	// surviving codex/store reader: rollout path resolution,
	// scanner walk, session-index open, and history-read failures.
	ConcernProviderCodexStore = "providers.codex.store"
	// ConcernProviderMITMLifecycle covers MITM proxy startup and
	// shutdown events.
	ConcernProviderMITMLifecycle = "providers.mitm.lifecycle"
	// ConcernProviderMITMWire covers per-frame and per-tunnel
	// wire-capture events (mitm.connect.tunnel spans,
	// mitm.capture.*, mitm.ws.*, mitm.baseline.*).
	ConcernProviderMITMWire = "providers.mitm.wire"
	// ConcernProviderMITMErrors covers MITM upstream and CONNECT
	// failures.
	ConcernProviderMITMErrors = "providers.mitm.errors"
)
