// Package correlation carries request, trace, and span identifiers across Clyde
// process boundaries.
package correlation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/grpc/metadata"
)

const (
	HeaderRequestID            = "x-clyde-request-id"
	HeaderTraceID              = "x-clyde-trace-id"
	HeaderSpanID               = "x-clyde-span-id"
	HeaderParentSpanID         = "x-clyde-parent-span-id"
	HeaderCursorRequestID      = "x-cursor-request-id"
	HeaderCursorConversationID = "x-cursor-conversation-id"
	HeaderCursorGenerationID   = "x-cursor-generation-id"
	HeaderUpstreamRequestID    = "x-upstream-request-id"
	HeaderUpstreamResponseID   = "x-upstream-response-id"
	HeaderTraceparent          = "traceparent"
	// HeaderClaudeCodeSessionID is set by claude-cli native ingress and used as
	// the per-chat key for native Anthropic ingress when present.
	HeaderClaudeCodeSessionID = "x-claude-code-session-id"
)

type TraceID string

type SpanID string

type Context struct {
	TraceID              TraceID
	SpanID               SpanID
	ParentSpanID         SpanID
	RequestID            string
	CursorRequestID      string
	CursorConversationID string
	CursorGenerationID   string
	UpstreamRequestID    string
	UpstreamResponseID   string
	// ChatKey is the per-chat partition key used by the transcript router.
	// Resolved at ingress from provider-specific headers (cursor conversation
	// id, claude-code session id) and back-filled from the request body's
	// metadata.cursorConversationId. Empty when no key resolves.
	ChatKey       string
	ChatKeySource string
	ChatRootKey   string
	ChatBranchKey string
}

type contextKey int

const correlationContextKey contextKey = 1

func New(requestID string) Context {
	return Context{
		TraceID:              NewTraceID(),
		SpanID:               NewSpanID(),
		ParentSpanID:         "",
		RequestID:            strings.TrimSpace(requestID),
		CursorRequestID:      "",
		CursorConversationID: "",
		CursorGenerationID:   "",
		UpstreamRequestID:    "",
		UpstreamResponseID:   "",
		ChatKey:              "",
		ChatKeySource:        "",
		ChatRootKey:          "",
		ChatBranchKey:        "",
	}
}

func FromHTTPHeader(header http.Header, requestID string) Context {
	corr := New(requestID)
	traceID, spanID, ok := ParseTraceparent(header.Get(HeaderTraceparent))
	if ok {
		corr.TraceID = traceID
		corr.ParentSpanID = spanID
		corr.SpanID = NewSpanID()
	}
	if traceID := strings.TrimSpace(header.Get(HeaderTraceID)); validTraceID(traceID) {
		corr.TraceID = TraceID(traceID)
	}
	if spanID := strings.TrimSpace(header.Get(HeaderSpanID)); validSpanID(spanID) {
		corr.ParentSpanID = SpanID(spanID)
		corr.SpanID = NewSpanID()
	}
	if parentSpanID := strings.TrimSpace(header.Get(HeaderParentSpanID)); validSpanID(parentSpanID) {
		corr.ParentSpanID = SpanID(parentSpanID)
	}
	corr.CursorRequestID = strings.TrimSpace(header.Get(HeaderCursorRequestID))
	corr.CursorConversationID = strings.TrimSpace(header.Get(HeaderCursorConversationID))
	corr.CursorGenerationID = strings.TrimSpace(header.Get(HeaderCursorGenerationID))
	corr.UpstreamRequestID = strings.TrimSpace(header.Get(HeaderUpstreamRequestID))
	corr.UpstreamResponseID = strings.TrimSpace(header.Get(HeaderUpstreamResponseID))
	// Resolve ChatKey from header at ingress. The first set value wins; an
	// already-set value in c (e.g. from a downstream call) is not overwritten
	// because this constructs a fresh Context.
	if conv := strings.TrimSpace(header.Get(HeaderCursorConversationID)); conv != "" {
		corr = corr.WithChatIdentity(conv, "native", conv, "")
	} else if sess := strings.TrimSpace(header.Get(HeaderClaudeCodeSessionID)); sess != "" {
		corr = corr.WithChatIdentity(sess, "native", sess, "")
	}
	return corr
}

func FromContext(ctx context.Context) Context {
	if ctx == nil {
		return emptyContext()
	}
	if corr, ok := ctx.Value(correlationContextKey).(Context); ok {
		return corr
	}
	return emptyContext()
}

func emptyContext() Context {
	return Context{
		TraceID:              "",
		SpanID:               "",
		ParentSpanID:         "",
		RequestID:            "",
		CursorRequestID:      "",
		CursorConversationID: "",
		CursorGenerationID:   "",
		UpstreamRequestID:    "",
		UpstreamResponseID:   "",
		ChatKey:              "",
		ChatKeySource:        "",
		ChatRootKey:          "",
		ChatBranchKey:        "",
	}
}

func WithContext(ctx context.Context, corr Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, correlationContextKey, corr)
}

func Ensure(ctx context.Context, requestID string) (context.Context, Context) {
	corr := FromContext(ctx)
	if corr.TraceID == "" {
		corr = New(requestID)
	}
	if corr.RequestID == "" {
		corr.RequestID = strings.TrimSpace(requestID)
	}
	return WithContext(ctx, corr), corr
}

func (c Context) WithRequestID(requestID string) Context {
	c.RequestID = strings.TrimSpace(requestID)
	return c
}

func (c Context) WithCursor(requestID, conversationID string) Context {
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		c.CursorRequestID = requestID
	}
	if conversationID = strings.TrimSpace(conversationID); conversationID != "" {
		c.CursorConversationID = conversationID
	}
	return c
}

func (c Context) WithCursorGenerationID(generationID string) Context {
	if generationID = strings.TrimSpace(generationID); generationID != "" {
		c.CursorGenerationID = generationID
	}
	return c
}

func (c Context) WithUpstreamRequestID(requestID string) Context {
	c.UpstreamRequestID = strings.TrimSpace(requestID)
	return c
}

func (c Context) WithUpstreamResponseID(responseID string) Context {
	c.UpstreamResponseID = strings.TrimSpace(responseID)
	return c
}

func (c Context) Child() Context {
	if c.TraceID == "" {
		c.TraceID = NewTraceID()
	}
	if c.SpanID != "" {
		c.ParentSpanID = c.SpanID
	}
	c.SpanID = NewSpanID()
	return c
}

func (c Context) Valid() bool {
	return validTraceID(string(c.TraceID)) && validSpanID(string(c.SpanID))
}

func (c Context) Traceparent() string {
	if !c.Valid() {
		return ""
	}
	return "00-" + string(c.TraceID) + "-" + string(c.SpanID) + "-01"
}

func (c Context) Attrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 8)
	if c.TraceID != "" {
		attrs = append(attrs, slog.String("trace_id", string(c.TraceID)))
	}
	if c.SpanID != "" {
		attrs = append(attrs, slog.String("span_id", string(c.SpanID)))
	}
	if c.ParentSpanID != "" {
		attrs = append(attrs, slog.String("parent_span_id", string(c.ParentSpanID)))
	}
	if c.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", c.RequestID))
	}
	if c.CursorRequestID != "" {
		attrs = append(attrs, slog.String("cursor_request_id", c.CursorRequestID))
	}
	if c.CursorConversationID != "" {
		attrs = append(attrs, slog.String("cursor_conversation_id", c.CursorConversationID))
	}
	if c.CursorGenerationID != "" {
		attrs = append(attrs, slog.String("cursor_generation_id", c.CursorGenerationID))
	}
	if c.UpstreamRequestID != "" {
		attrs = append(attrs, slog.String("upstream_request_id", c.UpstreamRequestID))
	}
	if c.UpstreamResponseID != "" {
		attrs = append(attrs, slog.String("upstream_response_id", c.UpstreamResponseID))
	}
	if c.ChatKey != "" {
		attrs = append(attrs, slog.String("chat_key", c.ChatKey))
	}
	if c.ChatKeySource != "" {
		attrs = append(attrs, slog.String("chat_key_source", c.ChatKeySource))
	}
	if c.ChatRootKey != "" {
		attrs = append(attrs, slog.String("chat_root_key", c.ChatRootKey))
	}
	if c.ChatBranchKey != "" {
		attrs = append(attrs, slog.String("chat_branch_key", c.ChatBranchKey))
	}
	return attrs
}

// WithChatKey returns a copy of c with ChatKey set to the trimmed value, but
// only if ChatKey is currently empty. A header-resolved key always wins over a
// later body-resolved key.
func (c Context) WithChatKey(key string) Context {
	key = strings.TrimSpace(key)
	if key == "" {
		return c
	}
	if c.ChatKey != "" {
		return c
	}
	c.ChatKey = key
	return c
}

// WithChatIdentity returns a copy of c carrying the resolved chat partition
// identity. It intentionally overwrites derived fields as a single unit after
// ingress resolution has chosen the native or derived path.
func (c Context) WithChatIdentity(key, source, rootKey, branchKey string) Context {
	c.ChatKey = strings.TrimSpace(key)
	c.ChatKeySource = strings.TrimSpace(source)
	c.ChatRootKey = strings.TrimSpace(rootKey)
	c.ChatBranchKey = strings.TrimSpace(branchKey)
	return c
}

func AttrsFromContext(ctx context.Context) []slog.Attr {
	return FromContext(ctx).Attrs()
}

func AppendAttrs(attrs []slog.Attr, corr Context) []slog.Attr {
	if len(attrs) == 0 {
		return corr.Attrs()
	}
	seen := make(map[string]bool, len(attrs))
	for _, attr := range attrs {
		seen[attr.Key] = true
	}
	for _, attr := range corr.Attrs() {
		if seen[attr.Key] {
			continue
		}
		attrs = append(attrs, attr)
		seen[attr.Key] = true
	}
	return attrs
}

func (c Context) SetHTTPHeaders(header http.Header) {
	if header == nil {
		return
	}
	if c.RequestID != "" {
		header.Set(HeaderRequestID, c.RequestID)
	}
	if c.TraceID != "" {
		header.Set(HeaderTraceID, string(c.TraceID))
	}
	if c.SpanID != "" {
		header.Set(HeaderSpanID, string(c.SpanID))
	}
	if c.ParentSpanID != "" {
		header.Set(HeaderParentSpanID, string(c.ParentSpanID))
	}
	if traceparent := c.Traceparent(); traceparent != "" {
		header.Set(HeaderTraceparent, traceparent)
	}
}

func (c Context) HTTPHeaders() http.Header {
	header := http.Header{}
	c.SetHTTPHeaders(header)
	if c.CursorRequestID != "" {
		header.Set(HeaderCursorRequestID, c.CursorRequestID)
	}
	if c.CursorConversationID != "" {
		header.Set(HeaderCursorConversationID, c.CursorConversationID)
	}
	if c.CursorGenerationID != "" {
		header.Set(HeaderCursorGenerationID, c.CursorGenerationID)
	}
	if c.UpstreamRequestID != "" {
		header.Set(HeaderUpstreamRequestID, c.UpstreamRequestID)
	}
	if c.UpstreamResponseID != "" {
		header.Set(HeaderUpstreamResponseID, c.UpstreamResponseID)
	}
	return header
}

func FromIncomingMetadata(ctx context.Context) Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return New("")
	}
	return fromMetadata(md)
}

func NewOutgoingContext(ctx context.Context) context.Context {
	corr := FromContext(ctx)
	if corr.TraceID == "" {
		_, corr = Ensure(ctx, "")
	}
	return metadata.NewOutgoingContext(ctx, corr.Metadata())
}

func (c Context) Metadata() metadata.MD {
	values := make(metadata.MD)
	setMetadata(values, HeaderRequestID, c.RequestID)
	setMetadata(values, HeaderTraceID, string(c.TraceID))
	setMetadata(values, HeaderSpanID, string(c.SpanID))
	setMetadata(values, HeaderParentSpanID, string(c.ParentSpanID))
	setMetadata(values, HeaderCursorRequestID, c.CursorRequestID)
	setMetadata(values, HeaderCursorConversationID, c.CursorConversationID)
	setMetadata(values, HeaderCursorGenerationID, c.CursorGenerationID)
	setMetadata(values, HeaderUpstreamRequestID, c.UpstreamRequestID)
	setMetadata(values, HeaderUpstreamResponseID, c.UpstreamResponseID)
	if traceparent := c.Traceparent(); traceparent != "" {
		setMetadata(values, HeaderTraceparent, traceparent)
	}
	return values
}

func ParseTraceparent(raw string) (TraceID, SpanID, bool) {
	parts := strings.Split(strings.TrimSpace(raw), "-")
	if len(parts) != 4 {
		return "", "", false
	}
	if parts[0] != "00" {
		return "", "", false
	}
	if !validTraceID(parts[1]) || !validSpanID(parts[2]) {
		return "", "", false
	}
	return TraceID(parts[1]), SpanID(parts[2]), true
}

func NewTraceID() TraceID {
	return TraceID(randomHex(16))
}

func NewSpanID() SpanID {
	return SpanID(randomHex(8))
}

func randomHex(byteCount int) string {
	buf := make([]byte, byteCount)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", byteCount*2)
	}
	return hex.EncodeToString(buf)
}

func fromMetadata(md metadata.MD) Context {
	corr := New(firstMetadata(md, HeaderRequestID))
	traceID, spanID, ok := ParseTraceparent(firstMetadata(md, HeaderTraceparent))
	if ok {
		corr.TraceID = traceID
		corr.ParentSpanID = spanID
		corr.SpanID = NewSpanID()
	}
	if traceID := firstMetadata(md, HeaderTraceID); validTraceID(traceID) {
		corr.TraceID = TraceID(traceID)
	}
	if spanID := firstMetadata(md, HeaderSpanID); validSpanID(spanID) {
		corr.ParentSpanID = SpanID(spanID)
		corr.SpanID = NewSpanID()
	}
	if parentSpanID := firstMetadata(md, HeaderParentSpanID); validSpanID(parentSpanID) {
		corr.ParentSpanID = SpanID(parentSpanID)
	}
	corr.CursorRequestID = firstMetadata(md, HeaderCursorRequestID)
	corr.CursorConversationID = firstMetadata(md, HeaderCursorConversationID)
	corr.CursorGenerationID = firstMetadata(md, HeaderCursorGenerationID)
	corr.UpstreamRequestID = firstMetadata(md, HeaderUpstreamRequestID)
	corr.UpstreamResponseID = firstMetadata(md, HeaderUpstreamResponseID)
	return corr
}

func firstMetadata(md metadata.MD, key string) string {
	values := md.Get(strings.ToLower(key))
	if len(values) == 0 {
		values = md.Get(key)
	}
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func setMetadata(md metadata.MD, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	md.Set(strings.ToLower(key), value)
}

func validTraceID(value string) bool {
	return validHex(value, 32) && value != strings.Repeat("0", 32)
}

func validSpanID(value string) bool {
	return validHex(value, 16) && value != strings.Repeat("0", 16)
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}
