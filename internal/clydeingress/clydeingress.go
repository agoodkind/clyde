// Package clydeingress carries the clyde-specific correlation surface
// that sits on top of [goodkind.io/gklog/correlation]. gklog owns the
// core trace, span, and request identifiers along with the standard
// HTTP header set and gRPC metadata. clydeingress adds the clyde wire
// fields the daemon and adapter speak today:
//
//   - The legacy x-clyde-* HTTP header set and x-upstream-* upstream
//     identifiers that flow on Cursor BYOK ingress and Codex egress.
//   - The x-claude-code-session-id header that native Anthropic ingress
//     uses to seed a per-chat key.
//   - The chat partition fields (chat_key, chat_key_source,
//     chat_root_key, chat_branch_key) and upstream-response fields
//     (upstream_request_id, upstream_response_id) that ride alongside
//     the trace.
//
// All clyde-specific fields are stored on the underlying
// [correlation.Context] as [correlation.IdentityAttribute] entries
// under stable keys (see [AttrKeyChatKey] etc.). The free functions
// here are the only intended way to read or set them, so call sites do
// not need to know which fields live in core gklog state and which are
// IdentityAttribute extensions.
package clydeingress

import (
	"net/http"
	"strings"

	"goodkind.io/gklog/correlation"
)

// Clyde-specific HTTP header names in canonical form. The daemon and
// adapter continue to read and write these instead of gklog's neutral
// x-request-id / x-trace-id set so external clients (Cursor BYOK,
// Codex sidecars) see the wire shape they have always seen.
const (
	HeaderRequestID    = "X-Clyde-Request-Id"
	HeaderTraceID      = "X-Clyde-Trace-Id"
	HeaderSpanID       = "X-Clyde-Span-Id"
	HeaderParentSpanID = "X-Clyde-Parent-Span-Id"
	HeaderTraceparent  = "Traceparent"

	// HeaderUpstreamRequestID and HeaderUpstreamResponseID propagate
	// upstream provider identifiers across adapter and renderer hops.
	HeaderUpstreamRequestID  = "X-Upstream-Request-Id"
	HeaderUpstreamResponseID = "X-Upstream-Response-Id"

	// HeaderClaudeCodeSessionID is set by claude-cli native ingress and
	// used as the per-chat key for native Anthropic ingress when present.
	HeaderClaudeCodeSessionID = "X-Claude-Code-Session-Id"
)

// IdentityAttribute keys used to store clyde-specific correlation
// fields on a [correlation.Context]. Call sites should prefer the
// typed accessors below over reading these keys directly.
const (
	AttrKeyChatKey            = "chat_key"
	AttrKeyChatKeySource      = "chat_key_source"
	AttrKeyChatRootKey        = "chat_root_key"
	AttrKeyChatBranchKey      = "chat_branch_key"
	AttrKeyUpstreamRequestID  = "upstream_request_id"
	AttrKeyUpstreamResponseID = "upstream_response_id"
)

// FromHTTPHeader builds a [correlation.Context] from header using
// gklog's standard parser for trace and span identifiers, then layers
// the clyde-specific x-clyde-* header set and x-claude-code-session-id
// on top. The fallback requestID is used only when no header carries
// one.
func FromHTTPHeader(header http.Header, requestID string) correlation.Context {
	rebuilt := http.Header{}
	if header != nil {
		copyIfPresent(rebuilt, header, HeaderTraceparent, correlation.HeaderTraceparent)
		copyIfPresent(rebuilt, header, HeaderRequestID, correlation.HeaderRequestID)
		copyIfPresent(rebuilt, header, HeaderTraceID, correlation.HeaderTraceID)
		copyIfPresent(rebuilt, header, HeaderSpanID, correlation.HeaderSpanID)
		copyIfPresent(rebuilt, header, HeaderParentSpanID, correlation.HeaderParentSpanID)
	}
	corr := correlation.FromHTTPHeader(rebuilt, requestID)
	if header == nil {
		return corr
	}
	if upstreamReq := strings.TrimSpace(header.Get(HeaderUpstreamRequestID)); upstreamReq != "" {
		corr = WithUpstreamRequestID(corr, upstreamReq)
	}
	if upstreamResp := strings.TrimSpace(header.Get(HeaderUpstreamResponseID)); upstreamResp != "" {
		corr = WithUpstreamResponseID(corr, upstreamResp)
	}
	if sess := strings.TrimSpace(header.Get(HeaderClaudeCodeSessionID)); sess != "" {
		corr = WithChatIdentity(corr, sess, "native", sess, "")
	}
	return corr
}

// SetHTTPHeaders writes the clyde-format header set onto header,
// covering the x-clyde-* trace fields, the standard W3C traceparent,
// the x-upstream-* upstream identifiers, and a header for the request
// id under both the clyde-specific key and gklog's neutral key. A nil
// header is a no-op.
func SetHTTPHeaders(corr correlation.Context, header http.Header) {
	if header == nil {
		return
	}
	if corr.RequestID != "" {
		header.Set(HeaderRequestID, corr.RequestID)
	}
	if corr.TraceID != "" {
		header.Set(HeaderTraceID, string(corr.TraceID))
	}
	if corr.SpanID != "" {
		header.Set(HeaderSpanID, string(corr.SpanID))
	}
	if corr.ParentSpanID != "" {
		header.Set(HeaderParentSpanID, string(corr.ParentSpanID))
	}
	if traceparent := corr.Traceparent(); traceparent != "" {
		header.Set(HeaderTraceparent, traceparent)
	}
}

// HTTPHeaders returns a fresh [http.Header] populated by
// [SetHTTPHeaders] plus the x-upstream-* headers when those fields are
// present on corr.
func HTTPHeaders(corr correlation.Context) http.Header {
	header := http.Header{}
	SetHTTPHeaders(corr, header)
	if id := UpstreamRequestID(corr); id != "" {
		header.Set(HeaderUpstreamRequestID, id)
	}
	if id := UpstreamResponseID(corr); id != "" {
		header.Set(HeaderUpstreamResponseID, id)
	}
	return header
}

// ChatKey returns the per-chat partition key on corr.
func ChatKey(corr correlation.Context) string {
	return corr.IdentityAttributeValue(AttrKeyChatKey)
}

// ChatKeySource returns the chat-key source on corr (native, derived, etc.).
func ChatKeySource(corr correlation.Context) string {
	return corr.IdentityAttributeValue(AttrKeyChatKeySource)
}

// ChatRootKey returns the chat root identifier on corr.
func ChatRootKey(corr correlation.Context) string {
	return corr.IdentityAttributeValue(AttrKeyChatRootKey)
}

// ChatBranchKey returns the chat branch identifier on corr.
func ChatBranchKey(corr correlation.Context) string {
	return corr.IdentityAttributeValue(AttrKeyChatBranchKey)
}

// UpstreamRequestID returns the upstream provider request id on corr.
func UpstreamRequestID(corr correlation.Context) string {
	return corr.IdentityAttributeValue(AttrKeyUpstreamRequestID)
}

// UpstreamResponseID returns the upstream provider response id on corr.
func UpstreamResponseID(corr correlation.Context) string {
	return corr.IdentityAttributeValue(AttrKeyUpstreamResponseID)
}

// WithChatKey returns a copy of corr with the per-chat partition key
// set to the trimmed value. The first non-empty key wins; a later
// WithChatKey call against the same corr is a no-op so a body-derived
// key cannot displace a header-derived one.
func WithChatKey(corr correlation.Context, key string) correlation.Context {
	key = strings.TrimSpace(key)
	if key == "" {
		return corr
	}
	if existing := ChatKey(corr); existing != "" {
		return corr
	}
	return corr.WithIdentityAttributes(correlation.IdentityAttribute{
		Key:   AttrKeyChatKey,
		Value: key,
	})
}

// WithChatIdentity returns a copy of corr carrying the resolved chat
// partition identity. It overwrites the chat-key, source, root, and
// branch fields as a unit after ingress resolution has chosen the
// native or derived path. Empty values clear the corresponding key.
func WithChatIdentity(corr correlation.Context, key, source, rootKey, branchKey string) correlation.Context {
	corr = setOrClear(corr, AttrKeyChatKey, key)
	corr = setOrClear(corr, AttrKeyChatKeySource, source)
	corr = setOrClear(corr, AttrKeyChatRootKey, rootKey)
	corr = setOrClear(corr, AttrKeyChatBranchKey, branchKey)
	return corr
}

// WithUpstreamRequestID returns a copy of corr with the upstream
// provider request id set to the trimmed value. An empty value clears
// the field.
func WithUpstreamRequestID(corr correlation.Context, requestID string) correlation.Context {
	return setOrClear(corr, AttrKeyUpstreamRequestID, requestID)
}

// WithUpstreamResponseID returns a copy of corr with the upstream
// provider response id set to the trimmed value. An empty value clears
// the field.
func WithUpstreamResponseID(corr correlation.Context, responseID string) correlation.Context {
	return setOrClear(corr, AttrKeyUpstreamResponseID, responseID)
}

// setOrClear sets the attribute when value is non-empty and clears it
// otherwise. Clearing a key is needed for WithChatIdentity, which is
// the one helper that intentionally rewrites the chat partition
// fields as a unit.
func setOrClear(corr correlation.Context, key, value string) correlation.Context {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" {
		return corr.WithIdentityAttributes(correlation.IdentityAttribute{Key: key, Value: trimmed})
	}
	return removeIdentityAttribute(corr, key)
}

// removeIdentityAttribute drops the named attribute from corr. The
// gklog API only supports merging attributes, not removing them, so
// this rebuilds the slice in place. The returned Context shares no
// memory with the input slice past the filter.
func removeIdentityAttribute(corr correlation.Context, key string) correlation.Context {
	if len(corr.IdentityAttributes) == 0 {
		return corr
	}
	filtered := make([]correlation.IdentityAttribute, 0, len(corr.IdentityAttributes))
	removed := false
	for _, attr := range corr.IdentityAttributes {
		if attr.Key == key {
			removed = true
			continue
		}
		filtered = append(filtered, attr)
	}
	if !removed {
		return corr
	}
	corr.IdentityAttributes = filtered
	return corr
}

func copyIfPresent(dst, src http.Header, sourceKey, destKey string) {
	if value := strings.TrimSpace(src.Get(sourceKey)); value != "" {
		dst.Set(destKey, value)
	}
}
