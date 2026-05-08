package slogger

// Provider concern constants (Claude, Codex, MITM).
const (
	ConcernProviderClaudeLifecycle     = "providers.claude.lifecycle"
	ConcernProviderClaudeDiscovery     = "providers.claude.discovery"
	ConcernProviderClaudeSettings      = "providers.claude.settings"
	ConcernProviderClaudeTranscript    = "providers.claude.transcript"
	ConcernProviderClaudeRemoteControl = "providers.claude.remote-control"
	ConcernProviderClaudeCleanup       = "providers.claude.cleanup"
	ConcernProviderClaudeWire          = "providers.claude.wire"
	ConcernProviderCodexLifecycle      = "providers.codex.lifecycle"
	ConcernProviderCodexDiscovery      = "providers.codex.discovery"
	ConcernProviderCodexTranscript     = "providers.codex.transcript"
	ConcernProviderCodexCleanup        = "providers.codex.cleanup"
	ConcernProviderCodexWire           = "providers.codex.wire"
	ConcernProviderMITMLifecycle       = "providers.mitm.lifecycle"
	ConcernProviderMITMWire            = "providers.mitm.wire"
	ConcernProviderMITMErrors          = "providers.mitm.errors"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernProviderClaudeLifecycle:     "providers/claude/lifecycle.jsonl",
		ConcernProviderClaudeDiscovery:     "providers/claude/discovery.jsonl",
		ConcernProviderClaudeSettings:      "providers/claude/settings.jsonl",
		ConcernProviderClaudeTranscript:    "providers/claude/transcript.jsonl",
		ConcernProviderClaudeRemoteControl: "providers/claude/remote-control.jsonl",
		ConcernProviderClaudeCleanup:       "providers/claude/cleanup.jsonl",
		ConcernProviderClaudeWire:          "providers/claude/wire.jsonl",
		ConcernProviderCodexLifecycle:      "providers/codex/lifecycle.jsonl",
		ConcernProviderCodexDiscovery:      "providers/codex/discovery.jsonl",
		ConcernProviderCodexTranscript:     "providers/codex/transcript.jsonl",
		ConcernProviderCodexCleanup:        "providers/codex/cleanup.jsonl",
		ConcernProviderCodexWire:           "providers/codex/wire.jsonl",
		ConcernProviderMITMLifecycle:       "providers/mitm/lifecycle.jsonl",
		ConcernProviderMITMWire:            "providers/mitm/wire.jsonl",
		ConcernProviderMITMErrors:          "providers/mitm/errors.jsonl",
	})

	registerEventConcernRules([]eventConcernRule{
		{"claude.lifecycle.", ConcernProviderClaudeLifecycle},
		{"claude.discovery.", ConcernProviderClaudeDiscovery},
		{"claude.settings.", ConcernProviderClaudeSettings},
		{"claude.transcript.", ConcernProviderClaudeTranscript},
		{"claude.remote", ConcernProviderClaudeRemoteControl},
		{"claude.cleanup.", ConcernProviderClaudeCleanup},
		{"claude.", ConcernProviderClaudeLifecycle},
		{"codex.lifecycle.", ConcernProviderCodexLifecycle},
		{"codex.discovery.", ConcernProviderCodexDiscovery},
		{"codex.transcript.", ConcernProviderCodexTranscript},
		{"codex.cleanup.", ConcernProviderCodexCleanup},
		{"codex.", ConcernProviderCodexLifecycle},
		{"mitm.proxy.started", ConcernProviderMITMLifecycle},
		{"mitm.launch.", ConcernProviderMITMLifecycle},
		{"mitm.connect.tunnel_", ConcernProviderMITMWire},
		{"mitm.capture.", ConcernProviderMITMWire},
		{"mitm.ws.", ConcernProviderMITMWire},
		{"mitm.baseline.", ConcernProviderMITMWire},
		{"mitm.proxy.upstream_failed", ConcernProviderMITMErrors},
		{"mitm.connect.", ConcernProviderMITMErrors},
		{"mitm.", ConcernProviderMITMWire},
	})
}
