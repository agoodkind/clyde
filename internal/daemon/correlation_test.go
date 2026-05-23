package daemon

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/gklog"
)

func TestDaemonDetachedCorrelationContextCreatesChildSpan(t *testing.T) {
	parentCorr := correlation.Context{
		TraceID:            correlation.TraceID("11111111111111111111111111111111"),
		SpanID:             correlation.SpanID("2222222222222222"),
		ParentSpanID:       "",
		RequestID:          "request-1",
		UpstreamRequestID:  "",
		UpstreamResponseID: "",
		ChatKey:            "",
		ChatKeySource:      "",
		ChatRootKey:        "",
		ChatBranchKey:      "",
	}
	parent := correlation.WithContext(context.Background(), parentCorr)

	detached := daemonDetachedCorrelationContext(parent, nil)
	got := correlation.FromContext(detached)

	if got.TraceID != parentCorr.TraceID {
		t.Fatalf("trace id = %q, want %q", got.TraceID, parentCorr.TraceID)
	}
	if got.ParentSpanID != parentCorr.SpanID {
		t.Fatalf("parent span id = %q, want %q", got.ParentSpanID, parentCorr.SpanID)
	}
	if got.SpanID == "" || got.SpanID == parentCorr.SpanID {
		t.Fatalf("span id = %q, want a fresh child span", got.SpanID)
	}
	if got.RequestID != parentCorr.RequestID {
		t.Fatalf("request id = %q, want %q", got.RequestID, parentCorr.RequestID)
	}
}

func TestLogDaemonRPCCompletedIncludesCorrelationAttrs(t *testing.T) {
	records := make([]daemonTestLogRecord, 0, 1)
	handler := &daemonTestLogHandler{records: &records}
	log := slog.New(handler)
	corr := correlation.Context{
		TraceID:            correlation.TraceID("33333333333333333333333333333333"),
		SpanID:             correlation.SpanID("4444444444444444"),
		ParentSpanID:       correlation.SpanID("5555555555555555"),
		RequestID:          "request-2",
		UpstreamRequestID:  "",
		UpstreamResponseID: "",
		ChatKey:            "",
		ChatKeySource:      "",
		ChatRootKey:        "",
		ChatBranchKey:      "",
		IdentityAttributes: []correlation.IdentityAttribute{
			{Key: "cursor_request_id", Value: "cursor-1"},
		},
	}
	ctx := gklog.WithLogger(correlation.WithContext(context.Background(), corr), log)

	logDaemonRPCCompleted(ctx, "/clyde.v1.ClydeService/ListSessions", time.Now(), nil)

	event := handler.last()
	if event.message != "daemon.rpc.completed" {
		t.Fatalf("message = %q, want daemon.rpc.completed", event.message)
	}
	assertDaemonLogAttr(t, event.attrs, "trace_id", string(corr.TraceID))
	assertDaemonLogAttr(t, event.attrs, "span_id", string(corr.SpanID))
	assertDaemonLogAttr(t, event.attrs, "parent_span_id", string(corr.ParentSpanID))
	assertDaemonLogAttr(t, event.attrs, "request_id", corr.RequestID)
	assertDaemonLogAttr(t, event.attrs, "cursor_request_id", "cursor-1")
}

func TestDaemonDiscoveryScanContextCreatesChildSpan(t *testing.T) {
	parentCorr := correlation.Context{
		TraceID:            correlation.TraceID("66666666666666666666666666666666"),
		SpanID:             correlation.SpanID("7777777777777777"),
		ParentSpanID:       "",
		RequestID:          "request-3",
		UpstreamRequestID:  "",
		UpstreamResponseID: "",
		ChatKey:            "",
		ChatKeySource:      "",
		ChatRootKey:        "",
		ChatBranchKey:      "",
	}

	ctx := daemonDiscoveryScanContext(discoveryScanSignal{
		Requested:   true,
		Correlation: parentCorr,
	}, nil)
	got := correlation.FromContext(ctx)

	if got.TraceID != parentCorr.TraceID {
		t.Fatalf("trace id = %q, want %q", got.TraceID, parentCorr.TraceID)
	}
	if got.ParentSpanID != parentCorr.SpanID {
		t.Fatalf("parent span id = %q, want %q", got.ParentSpanID, parentCorr.SpanID)
	}
	if got.SpanID == "" || got.SpanID == parentCorr.SpanID {
		t.Fatalf("span id = %q, want a fresh discovery span", got.SpanID)
	}
	if got.RequestID != parentCorr.RequestID {
		t.Fatalf("request id = %q, want %q", got.RequestID, parentCorr.RequestID)
	}
}

type daemonTestLogHandler struct {
	attrs   []slog.Attr
	records *[]daemonTestLogRecord
}

type daemonTestLogRecord struct {
	message string
	attrs   []slog.Attr
}

func (h *daemonTestLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *daemonTestLogHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := append([]slog.Attr(nil), h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	*h.records = append(*h.records, daemonTestLogRecord{
		message: record.Message,
		attrs:   attrs,
	})
	return nil
}

func (h *daemonTestLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &daemonTestLogHandler{
		attrs:   append([]slog.Attr(nil), h.attrs...),
		records: h.records,
	}
	next.attrs = append(next.attrs, attrs...)
	return next
}

func (h *daemonTestLogHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *daemonTestLogHandler) last() daemonTestLogRecord {
	if h.records == nil || len(*h.records) == 0 {
		return daemonTestLogRecord{}
	}
	return (*h.records)[len(*h.records)-1]
}

func assertDaemonLogAttr(t *testing.T, attrs []slog.Attr, key, want string) {
	t.Helper()
	for _, attr := range attrs {
		if attr.Key == key {
			if got := attr.Value.String(); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("missing log attr %q", key)
}
