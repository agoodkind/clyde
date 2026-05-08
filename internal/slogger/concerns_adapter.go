package slogger

// Adapter concern constants (HTTP, models, chat, provider bridges).
const (
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
)

func init() {
	registerConcernPaths(map[string]string{
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
	})

	registerEventConcernRules([]eventConcernRule{
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
	})
}
