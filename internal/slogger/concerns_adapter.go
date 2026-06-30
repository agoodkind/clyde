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
	ConcernAdapterProviderPassthroughReq  = "adapter.providers.passthrough_override.request"
	ConcernAdapterProviderPassthroughCoer = "adapter.providers.passthrough_override.coercion"
)
