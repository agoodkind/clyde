package response

import (
	"context"
	"testing"

	"goodkind.io/gklog/correlation"
)

func TestJSONInjectsMetadataIntoObjectPayload(t *testing.T) {
	t.Parallel()
	corr := correlation.Context{
		RequestID: "req-456",
		TraceID:   correlation.TraceID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		SpanID:    correlation.SpanID("bbbbbbbbbbbbbbbb"),
	}
	ctx := correlation.WithContext(context.Background(), corr)

	got, err := JSON(ctx, []byte("{\"status\":\"ok\"}\n"), JSONCompact)
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	want := "{\"_meta\":{\"request_id\":\"req-456\",\"trace_id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"span_id\":\"bbbbbbbbbbbbbbbb\"},\"status\":\"ok\"}\n"
	if string(got) != want {
		t.Fatalf("JSON() = %q, want %q", got, want)
	}
}

func TestJSONWrapsScalarPayloadInResponseEnvelope(t *testing.T) {
	t.Parallel()
	corr := correlation.Context{
		TraceID: correlation.TraceID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		SpanID:  correlation.SpanID("bbbbbbbbbbbbbbbb"),
	}
	ctx := correlation.WithContext(context.Background(), corr)

	got, err := JSON(ctx, []byte("\"ok\"\n"), JSONCompact)
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	want := "{\"_meta\":{\"trace_id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"span_id\":\"bbbbbbbbbbbbbbbb\"},\"result\":\"ok\"}\n"
	if string(got) != want {
		t.Fatalf("JSON() = %q, want %q", got, want)
	}
}
