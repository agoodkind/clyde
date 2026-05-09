package livetrack

import (
	"context"
	"log/slog"

	"goodkind.io/clyde/internal/correlation"
)

// emitSessionEvent logs one register/release event with the canonical
// field set. Session events do not carry drain-specific fields; the
// drain events have their own helper.
func (r *Registry[M]) emitSessionEvent(ctx context.Context, level slog.Level, message string, sess *Session[M], reason string) {
	if r.log == nil {
		return
	}
	openedMsAgo := r.now().Sub(sess.OpenedAt).Milliseconds()
	attrs := []slog.Attr{
		slog.String("session_id", sess.ID),
		slog.String("parent_id", sess.ParentID),
		slog.String("kind", sess.Kind),
		slog.Int64("opened_ms_ago", openedMsAgo),
		slog.String("reason", reason),
	}
	attrs = appendCorrelationAttrs(attrs, ctx, sess.corr)
	r.log.LogAttrs(ctx, level, message, attrs...)
}

// emitForceCloseEvent logs the warn-level force_close event for one
// session. It replays the session's captured correlation context so
// the record carries the trace_id of whoever opened the session, not
// the trace_id of the drain caller. ctx is the caller-supplied
// context used only for slog routing; correlation comes from the
// session itself.
func (r *Registry[M]) emitForceCloseEvent(ctx context.Context, sess *Session[M], reason string) {
	if r.log == nil {
		return
	}
	openedMsAgo := r.now().Sub(sess.OpenedAt).Milliseconds()
	attrs := []slog.Attr{
		slog.String("session_id", sess.ID),
		slog.String("parent_id", sess.ParentID),
		slog.String("kind", sess.Kind),
		slog.Int64("opened_ms_ago", openedMsAgo),
		slog.String("reason", reason),
	}
	attrs = appendCorrelationAttrs(attrs, ctx, sess.corr)
	r.log.LogAttrs(ctx, slog.LevelWarn, "livetrack.session.force_close", attrs...)
}

// emitDrainStarted logs the info-level drain.started event.
func (r *Registry[M]) emitDrainStarted(ctx context.Context, reason string) {
	if r.log == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("reason", reason),
		slog.String("state", StateDraining.String()),
		slog.Int("remaining", r.Count()),
	}
	attrs = appendCorrelationAttrsCtxOnly(attrs, ctx)
	r.log.LogAttrs(ctx, slog.LevelInfo, "livetrack.drain.started", attrs...)
}

// emitDrainComplete logs the info-level drain.complete event with the
// terminal state and force-close accounting.
func (r *Registry[M]) emitDrainComplete(ctx context.Context, result DrainResult) {
	if r.log == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("reason", result.Reason),
		slog.String("state", result.Final.String()),
		slog.Int("remaining", result.Remaining),
		slog.Int("force_closed", result.ForceClosed),
		slog.Int64("duration_ms", result.Duration.Milliseconds()),
	}
	attrs = appendCorrelationAttrsCtxOnly(attrs, ctx)
	r.log.LogAttrs(ctx, slog.LevelInfo, "livetrack.drain.complete", attrs...)
}

// appendCorrelationAttrs merges correlation attributes from both the
// caller context and the session-captured correlation. Caller
// attributes win when both are present so drain events are tied to
// the drain caller while session events stay tied to the registering
// caller. The session correlation back-fills missing fields.
func appendCorrelationAttrs(attrs []slog.Attr, ctx context.Context, sessCorr correlation.Context) []slog.Attr {
	ctxCorr := correlation.FromContext(ctx)
	attrs = correlation.AppendAttrs(attrs, ctxCorr)
	attrs = correlation.AppendAttrs(attrs, sessCorr)
	return attrs
}

// appendCorrelationAttrsCtxOnly appends correlation attributes from
// the caller context only. Drain-level events have no per-session
// correlation to back-fill, so this helper avoids constructing an
// empty correlation.Context literal at the call site.
func appendCorrelationAttrsCtxOnly(attrs []slog.Attr, ctx context.Context) []slog.Attr {
	return correlation.AppendAttrs(attrs, correlation.FromContext(ctx))
}
