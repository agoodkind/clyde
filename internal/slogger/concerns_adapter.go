package slogger

// Adapter concern constants (HTTP, models, chat, provider bridges).
const (
	ConcernAdapterHTTPIngress             = "adapter.http.ingress"
	ConcernAdapterHTTPEgress              = "adapter.http.egress"
	ConcernAdapterHTTPErrors              = "adapter.http.errors"
	ConcernAdapterModelsCatalog           = "adapter.models.catalog"
	ConcernAdapterModelsResolve           = "adapter.models.resolve"
	ConcernAdapterModelsCursor            = "adapter.models.cursor"
	ConcernAdapterChatPreflight           = "adapter.chat.preflight"
	ConcernAdapterChatDispatch            = "adapter.chat.dispatch"
	ConcernAdapterChatRender              = "adapter.chat.render"
	ConcernAdapterNotice                  = "adapter.notice"
	ConcernAdapterProviderCodex           = "adapter.providers.codex.request"
	ConcernAdapterProviderCodexWS         = "adapter.providers.codex.websocket"
	ConcernAdapterProviderCodexSess       = "adapter.providers.codex.session-reuse"
	ConcernAdapterProviderCodexErr        = "adapter.providers.codex.errors"
	ConcernAdapterProviderAnthReq         = "adapter.providers.anthropic.request"
	ConcernAdapterProviderAnthSSE         = "adapter.providers.anthropic.sse"
	ConcernAdapterProviderAnthOAuth       = "adapter.providers.anthropic.oauth"
	ConcernAdapterProviderAnthErr         = "adapter.providers.anthropic.errors"
	ConcernAdapterProviderAnthWire        = "adapter.providers.anthropic.wire_capture"
	ConcernAdapterProviderCodexWire       = "adapter.providers.codex.wire_capture"
	ConcernAdapterProviderPassthroughReq  = "adapter.providers.passthrough_override.request"
	ConcernAdapterProviderPassthroughCoer = "adapter.providers.passthrough_override.coercion"
)

func init() {
	registerConcernPaths(adapterConcernPaths())
}

func adapterConcernPaths() map[string]string {
	return map[string]string{
		ConcernAdapterHTTPIngress:             "adapter/http/ingress.jsonl",
		ConcernAdapterHTTPEgress:              "adapter/http/egress.jsonl",
		ConcernAdapterHTTPErrors:              "adapter/http/errors.jsonl",
		ConcernAdapterModelsCatalog:           "adapter/models/catalog.jsonl",
		ConcernAdapterModelsResolve:           "adapter/models/resolve.jsonl",
		ConcernAdapterModelsCursor:            "adapter/models/cursor.jsonl",
		ConcernAdapterChatPreflight:           "adapter/chat/preflight.jsonl",
		ConcernAdapterChatDispatch:            "adapter/chat/dispatch.jsonl",
		ConcernAdapterChatRender:              "adapter/chat/render.jsonl",
		ConcernAdapterNotice:                  "adapter/notice.jsonl",
		ConcernAdapterProviderCodex:           "adapter/providers/codex/request.jsonl",
		ConcernAdapterProviderCodexWS:         "adapter/providers/codex/websocket.jsonl",
		ConcernAdapterProviderCodexSess:       "adapter/providers/codex/session-reuse.jsonl",
		ConcernAdapterProviderCodexErr:        "adapter/providers/codex/errors.jsonl",
		ConcernAdapterProviderAnthReq:         "adapter/providers/anthropic/request.jsonl",
		ConcernAdapterProviderAnthSSE:         "adapter/providers/anthropic/sse.jsonl",
		ConcernAdapterProviderAnthOAuth:       "adapter/providers/anthropic/oauth.jsonl",
		ConcernAdapterProviderAnthErr:         "adapter/providers/anthropic/errors.jsonl",
		ConcernAdapterProviderAnthWire:        "adapter/providers/anthropic/wire_capture.jsonl",
		ConcernAdapterProviderCodexWire:       "adapter/providers/codex/wire_capture.jsonl",
		ConcernAdapterProviderPassthroughReq:  "adapter/providers/passthrough_override/request.jsonl",
		ConcernAdapterProviderPassthroughCoer: "adapter/providers/passthrough_override/coercion.jsonl",
	}
}
