package slogger

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/gklog"
)

const (
	ConcernProcessDaemonLifecycle = "process.daemon.lifecycle"
	ConcernProcessDaemonLocks     = "process.daemon.locks"
	ConcernProcessDaemonListeners = "process.daemon.listeners"
	ConcernProcessDaemonConfig    = "process.daemon.config"

	ConcernCmdDispatch = "cmd.dispatch"
	ConcernCmdResume   = "cmd.resume"
	ConcernCmdCompact  = "cmd.compact"

	ConcernSessionDomainStore        = "session.domain.store"
	ConcernSessionDomainResolve      = "session.domain.resolve"
	ConcernSessionDomainSearch       = "session.domain.search"
	ConcernSessionDomainCapabilities = "session.domain.capabilities"
	ConcernSessionLifecycleLaunch    = "session.lifecycle.launch"
	ConcernSessionLifecycleRuntime   = "session.lifecycle.runtime"
	ConcernSessionLifecycleCleanup   = "session.lifecycle.cleanup"
	ConcernSessionDiscoveryScan      = "session.discovery.scan"
	ConcernSessionDiscoveryAdopt     = "session.discovery.adopt"

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

	ConcernAdapterHTTPIngress             = "adapter.http.ingress"
	ConcernAdapterHTTPEgress              = "adapter.http.egress"
	ConcernAdapterHTTPRaw                 = "adapter.http.raw"
	ConcernAdapterHTTPErrors              = "adapter.http.errors"
	ConcernAdapterModelsCatalog           = "adapter.models.catalog"
	ConcernAdapterModelsResolve           = "adapter.models.resolve"
	ConcernAdapterModelsCursor            = "adapter.models.cursor"
	ConcernAdapterChatDiscovery           = "adapter.chat.discovery"
	ConcernAdapterChatPreflight           = "adapter.chat.preflight"
	ConcernAdapterChatDispatch            = "adapter.chat.dispatch"
	ConcernAdapterChatRender              = "adapter.chat.render"
	ConcernAdapterNotice                  = "adapter.notice"
	ConcernAdapterProviderCodex           = "adapter.providers.codex.request"
	ConcernAdapterProviderCodexWS         = "adapter.providers.codex.websocket"
	ConcernAdapterProviderCodexSess       = "adapter.providers.codex.session-reuse"
	ConcernAdapterProviderCodexResp       = "adapter.providers.codex.responses"
	ConcernAdapterProviderCodexErr        = "adapter.providers.codex.errors"
	ConcernAdapterProviderAnthReq         = "adapter.providers.anthropic.request"
	ConcernAdapterProviderAnthSSE         = "adapter.providers.anthropic.sse"
	ConcernAdapterProviderAnthOAuth       = "adapter.providers.anthropic.oauth"
	ConcernAdapterProviderAnthErr         = "adapter.providers.anthropic.errors"
	ConcernAdapterProviderAnthWire        = "adapter.providers.anthropic.wire_capture"
	ConcernAdapterProviderCodexWire       = "adapter.providers.codex.wire_capture"
	ConcernAdapterProviderPassthroughReq  = "adapter.providers.passthrough_override.request"
	ConcernAdapterProviderPassthroughCoer = "adapter.providers.passthrough_override.coercion"
	ConcernAdapterProviderPassthroughErr  = "adapter.providers.passthrough_override.errors"
	// ConcernAdapterReasoningConfig is the slog concern for startup
	// notices emitted when the per-provider reasoning config blocks
	// fold legacy [adapter.synthetic_content] values forward.
	ConcernAdapterReasoningConfig = "adapter.reasoning.config"

	ConcernDaemonRPCRequests       = "daemon.rpc.requests"
	ConcernDaemonRPCStreams        = "daemon.rpc.streams"
	ConcernDaemonWorkersPrune      = "daemon.workers.prune"
	ConcernDaemonWorkersBridge     = "daemon.workers.bridge-watch"
	ConcernDaemonWorkersTranscript = "daemon.workers.transcript-hub"
	ConcernDaemonWorkersReload     = "daemon.workers.reload"

	ConcernUITUILifecycle   = "ui.tui.lifecycle"
	ConcernUITUIActions     = "ui.tui.actions"
	ConcernUITUIRenderErr   = "ui.tui.render-errors"
	ConcernUISidecarTail    = "ui.sidecar.tail"
	ConcernUISidecarSend    = "ui.sidecar.send"
	ConcernMCPServerRequest = "mcp.server.requests"
	ConcernMCPServerSearch  = "mcp.server.search"
	ConcernMCPServerContext = "mcp.server.context"
	ConcernMCPServerErrors  = "mcp.server.errors"
	ConcernCompactPreview   = "compact.preview"
	ConcernCompactApply     = "compact.apply"
	ConcernCompactUndo      = "compact.undo"
	ConcernCompactLedger    = "compact.ledger"
	ConcernCompactCalib     = "compact.calibration"
)

var concernPaths = map[string]string{
	ConcernProcessDaemonLifecycle: "process/daemon/lifecycle.jsonl",
	ConcernProcessDaemonLocks:     "process/daemon/locks.jsonl",
	ConcernProcessDaemonListeners: "process/daemon/listeners.jsonl",
	ConcernProcessDaemonConfig:    "process/daemon/config.jsonl",
	ConcernCmdDispatch:            "cmd/dispatch.jsonl",
	ConcernCmdResume:              "cmd/resume.jsonl",
	ConcernCmdCompact:             "cmd/compact.jsonl",

	ConcernSessionDomainStore:        "session/domain/store.jsonl",
	ConcernSessionDomainResolve:      "session/domain/resolve.jsonl",
	ConcernSessionDomainSearch:       "session/domain/search.jsonl",
	ConcernSessionDomainCapabilities: "session/domain/capabilities.jsonl",
	ConcernSessionLifecycleLaunch:    "session/lifecycle/launch.jsonl",
	ConcernSessionLifecycleRuntime:   "session/lifecycle/runtime.jsonl",
	ConcernSessionLifecycleCleanup:   "session/lifecycle/cleanup.jsonl",
	ConcernSessionDiscoveryScan:      "session/discovery/scan.jsonl",
	ConcernSessionDiscoveryAdopt:     "session/discovery/adopt.jsonl",

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

	ConcernAdapterHTTPIngress:             "adapter/http/ingress.jsonl",
	ConcernAdapterHTTPEgress:              "adapter/http/egress.jsonl",
	ConcernAdapterHTTPRaw:                 "adapter/http/raw.jsonl",
	ConcernAdapterHTTPErrors:              "adapter/http/errors.jsonl",
	ConcernAdapterModelsCatalog:           "adapter/models/catalog.jsonl",
	ConcernAdapterModelsResolve:           "adapter/models/resolve.jsonl",
	ConcernAdapterModelsCursor:            "adapter/models/cursor.jsonl",
	ConcernAdapterChatDiscovery:           "adapter/chat/discovery.jsonl",
	ConcernAdapterChatPreflight:           "adapter/chat/preflight.jsonl",
	ConcernAdapterChatDispatch:            "adapter/chat/dispatch.jsonl",
	ConcernAdapterChatRender:              "adapter/chat/render.jsonl",
	ConcernAdapterNotice:                  "adapter/notice.jsonl",
	ConcernAdapterProviderCodex:           "adapter/providers/codex/request.jsonl",
	ConcernAdapterProviderCodexWS:         "adapter/providers/codex/websocket.jsonl",
	ConcernAdapterProviderCodexSess:       "adapter/providers/codex/session-reuse.jsonl",
	ConcernAdapterProviderCodexResp:       "adapter/providers/codex/responses.jsonl",
	ConcernAdapterProviderCodexErr:        "adapter/providers/codex/errors.jsonl",
	ConcernAdapterProviderAnthReq:         "adapter/providers/anthropic/request.jsonl",
	ConcernAdapterProviderAnthSSE:         "adapter/providers/anthropic/sse.jsonl",
	ConcernAdapterProviderAnthOAuth:       "adapter/providers/anthropic/oauth.jsonl",
	ConcernAdapterProviderAnthErr:         "adapter/providers/anthropic/errors.jsonl",
	ConcernAdapterProviderAnthWire:        "adapter/providers/anthropic/wire_capture.jsonl",
	ConcernAdapterProviderCodexWire:       "adapter/providers/codex/wire_capture.jsonl",
	ConcernAdapterProviderPassthroughReq:  "adapter/providers/passthrough_override/request.jsonl",
	ConcernAdapterProviderPassthroughCoer: "adapter/providers/passthrough_override/coercion.jsonl",
	ConcernAdapterProviderPassthroughErr:  "adapter/providers/passthrough_override/errors.jsonl",
	ConcernAdapterReasoningConfig:         "adapter/reasoning/config.jsonl",

	ConcernDaemonRPCRequests:       "daemon/rpc/requests.jsonl",
	ConcernDaemonRPCStreams:        "daemon/rpc/streams.jsonl",
	ConcernDaemonWorkersPrune:      "daemon/workers/prune.jsonl",
	ConcernDaemonWorkersBridge:     "daemon/workers/bridge-watch.jsonl",
	ConcernDaemonWorkersTranscript: "daemon/workers/transcript-hub.jsonl",
	ConcernDaemonWorkersReload:     "daemon/workers/reload.jsonl",
	ConcernUITUILifecycle:          "ui/tui/lifecycle.jsonl",
	ConcernUITUIActions:            "ui/tui/actions.jsonl",
	ConcernUITUIRenderErr:          "ui/tui/render-errors.jsonl",
	ConcernUISidecarTail:           "ui/sidecar/tail.jsonl",
	ConcernUISidecarSend:           "ui/sidecar/send.jsonl",
	ConcernMCPServerRequest:        "mcp/server/requests.jsonl",
	ConcernMCPServerSearch:         "mcp/server/search.jsonl",
	ConcernMCPServerContext:        "mcp/server/context.jsonl",
	ConcernMCPServerErrors:         "mcp/server/errors.jsonl",
	ConcernCompactPreview:          "compact/preview.jsonl",
	ConcernCompactApply:            "compact/apply.jsonl",
	ConcernCompactUndo:             "compact/undo.jsonl",
	ConcernCompactLedger:           "compact/ledger.jsonl",
	ConcernCompactCalib:            "compact/calibration.jsonl",
}

// IsKnownConcern reports whether concern is registered by the concern file
// router.
func IsKnownConcern(concern string) bool {
	_, ok := concernPaths[strings.TrimSpace(concern)]
	return ok
}

// ConcernRelPath returns the relative path under the concern root for the
// named concern. Callers join the result with [DefaultConcernRoot] to obtain
// the absolute on-disk JSONL path. The returned path is empty when the concern
// is unknown.
func ConcernRelPath(concern string) string {
	return concernPaths[strings.TrimSpace(concern)]
}

// eventConcernRules is kept as a migration reference for older event names.
// The production concern router no longer depends on it; records must carry
// the explicit concern attr from slogger.For/WithConcern or a narrow
// import-cycle-safe equivalent.
type eventConcernRule struct {
	prefix  string
	concern string
}

var eventConcernRules = []eventConcernRule{
	{"adapter.models.listed", ConcernAdapterModelsCatalog},
	{"adapter.request.raw", ConcernAdapterHTTPRaw},
	{"adapter.chat.raw", ConcernAdapterHTTPRaw},
	{"adapter.request.panic", ConcernAdapterHTTPErrors},
	{"adapter.chat.panic", ConcernAdapterHTTPErrors},
	{"adapter.chat.parse_failed", ConcernAdapterHTTPErrors},
	{"adapter.chat.validation_failed", ConcernAdapterChatPreflight},
	{"adapter.preflight.", ConcernAdapterChatPreflight},
	{"adapter.chat.ingress", ConcernAdapterHTTPIngress},
	{"adapter.chat.discovery", ConcernAdapterChatDiscovery},
	{"adapter.tools.normalized", ConcernAdapterChatDiscovery},
	{"adapter.messages.normalized", ConcernAdapterChatDiscovery},
	{"adapter.messages.normalize_failed", ConcernAdapterChatPreflight},
	{"adapter.model.", ConcernAdapterModelsResolve},
	{"adapter.resolver.", ConcernAdapterModelsResolve},
	{"adapter.cursor.", ConcernAdapterModelsCursor},
	{"adapter.backend.", ConcernAdapterChatDispatch},
	{"adapter.chat.received", ConcernAdapterChatDispatch},
	{"adapter.chat.completed", ConcernAdapterChatRender},
	{"adapter.chat.stream_", ConcernAdapterChatRender},
	{"adapter.cache.", ConcernAdapterChatRender},
	{"adapter.codex.provider_error", ConcernAdapterProviderCodexErr},
	{"adapter.codex.transport.", ConcernAdapterProviderCodexWS},
	{"adapter.codex.session", ConcernAdapterProviderCodexSess},
	{"adapter.codex.response.", ConcernAdapterProviderCodexResp},
	{"adapter.codex.", ConcernAdapterProviderCodex},
	{"adapter.anthropic.oauth", ConcernAdapterProviderAnthOAuth},
	{"adapter.anthropic.provider_error", ConcernAdapterProviderAnthErr},
	{"adapter.anthropic.error", ConcernAdapterProviderAnthErr},
	{"adapter.anthropic.sse", ConcernAdapterProviderAnthSSE},
	{"adapter.anthropic.", ConcernAdapterProviderAnthReq},
	{"adapter.passthrough_override.", ConcernAdapterProviderPassthroughReq},
	{"adapter.notice.", ConcernAdapterNotice},
	{"adapter listening", ConcernProcessDaemonListeners},

	{"daemon.rpc.stream_", ConcernDaemonRPCStreams},
	{"daemon.rpc.", ConcernDaemonRPCRequests},
	{"daemon.reload.", ConcernDaemonWorkersReload},
	{"daemon.worker.reload", ConcernDaemonWorkersReload},
	{"daemon.bridge.", ConcernDaemonWorkersBridge},
	{"bridge.", ConcernDaemonWorkersBridge},
	{"transcript_hub.", ConcernDaemonWorkersTranscript},
	{"provider_stats.", ConcernDaemonWorkersTranscript},
	{"daemon.", ConcernProcessDaemonLifecycle},

	{"session.scan.", ConcernSessionDiscoveryScan},
	{"session.adopt.", ConcernSessionDiscoveryAdopt},
	{"session.resolve.", ConcernSessionDomainResolve},
	{"session.store.", ConcernSessionDomainStore},
	{"session.list.", ConcernSessionDomainSearch},
	{"session.search.", ConcernSessionDomainSearch},
	{"session.context.", ConcernSessionDomainCapabilities},
	{"session.lifecycle.", ConcernSessionLifecycleRuntime},
	{"session.cleanup.", ConcernSessionLifecycleCleanup},
	{"session.new.", ConcernSessionLifecycleLaunch},
	{"session.resume.", ConcernSessionLifecycleLaunch},

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

	{"cli.args.", ConcernCmdDispatch},
	{"cli.execute.", ConcernCmdDispatch},
	{"cli.main.", ConcernCmdDispatch},
	{"cli.resume.", ConcernCmdResume},
	{"cmd.session.", ConcernCmdResume},
	{"forward.", ConcernCmdDispatch},
	{"dashboard.", ConcernUITUILifecycle},

	{"tui.sidecar.tail", ConcernUISidecarTail},
	{"tui.sidecar.send", ConcernUISidecarSend},
	{"sidecar.", ConcernUISidecarSend},
	{"tui.input.", ConcernUITUIActions},
	{"tui.event.", ConcernUITUIActions},
	{"tui.overlay.", ConcernUITUIActions},
	{"resume.row_selected", ConcernUITUIActions},
	{"returnprompt.", ConcernUITUIActions},
	{"tui.draw.", ConcernUITUIRenderErr},
	{"tui.loop.event_timing", ConcernUITUIRenderErr},
	{"tui.table.populate_timing", ConcernUITUIRenderErr},
	{"tui.signal.", ConcernUITUIRenderErr},
	{"tui.", ConcernUITUILifecycle},
	{"resume.start", ConcernSessionLifecycleLaunch},
	{"resume.exit", ConcernSessionLifecycleLaunch},

	{"mcp.server.", ConcernMCPServerRequest},
	{"mcp.search.", ConcernMCPServerSearch},
	{"mcp.context.", ConcernMCPServerContext},
	{"mcp.error", ConcernMCPServerErrors},
	{"analyze_results", ConcernMCPServerSearch},

	{"compact.preview.", ConcernCompactPreview},
	{"compact.apply.", ConcernCompactApply},
	{"compact.undo.", ConcernCompactUndo},
	{"compact.ledger.", ConcernCompactLedger},
	{"compact.calibration.", ConcernCompactCalib},
	{"compact.", ConcernCmdCompact},
	{"prune.autoname.", ConcernDaemonWorkersPrune},
	{"prune.delete.", ConcernDaemonWorkersPrune},
	{"prune.", ConcernDaemonWorkersPrune},
}

func concernForEvent(message string) string {
	for _, rule := range eventConcernRules {
		if strings.HasPrefix(message, rule.prefix) {
			return rule.concern
		}
	}
	return ""
}

func concernHandlers(root string, level slog.Level, rotation RotationPolicy, policies map[string]ConcernPolicy) []slog.Handler {
	handlers := make([]slog.Handler, 0, len(concernPaths))
	for concern, rel := range concernPaths {
		handlerLevel := level
		rot := gklog.RotationConfig{}
		if rotation.Enabled {
			rot = rotationConfig(rotation)
		}
		policy, ok := policies[concern]
		if ok && policy.Enabled != nil && !*policy.Enabled {
			continue
		}
		if ok && policy.Level != nil {
			handlerLevel = *policy.Level
		}
		if ok && policy.Rotation != nil {
			rot = rotationConfigForConcern(*policy.Rotation)
		}
		path := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		handlers = append(handlers, newConcernFilterHandler(concern, gklog.FileJSON(path, handlerLevel, rot)))
	}
	return handlers
}

func rotationConfigForConcern(policy RotationPolicy) gklog.RotationConfig {
	if !policy.Enabled {
		return gklog.RotationConfig{}
	}
	return rotationConfig(policy)
}
