package response

import (
	"bytes"
	"context"
	"testing"

	"goodkind.io/gklog/correlation"
)

func TestTextPlacesMetadataFirst(t *testing.T) {
	t.Parallel()
	corr := correlation.Context{
		RequestID:    "req-123",
		TraceID:      correlation.TraceID("11111111111111111111111111111111"),
		SpanID:       correlation.SpanID("2222222222222222"),
		ParentSpanID: correlation.SpanID("3333333333333333"),
	}
	ctx := correlation.WithContext(context.Background(), corr)

	got := Text(ctx, "payload\n")
	want := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222 parent_span_id=3333333333333333 request_id=req-123\npayload\n"
	if got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
}

func TestWriteHeaderLineWritesMetadataWhenPresent(t *testing.T) {
	t.Parallel()
	corr := correlation.Context{
		RequestID:    "req-123",
		TraceID:      correlation.TraceID("11111111111111111111111111111111"),
		SpanID:       correlation.SpanID("2222222222222222"),
		ParentSpanID: correlation.SpanID("3333333333333333"),
	}
	ctx := correlation.WithContext(context.Background(), corr)

	var out bytes.Buffer
	if err := WriteHeaderLine(ctx, &out); err != nil {
		t.Fatalf("WriteHeaderLine() error = %v", err)
	}

	want := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222 parent_span_id=3333333333333333 request_id=req-123\n"
	if got := out.String(); got != want {
		t.Fatalf("WriteHeaderLine() wrote %q, want %q", got, want)
	}
}

func TestWriteHeaderLineWritesNothingWhenMetadataAbsent(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := WriteHeaderLine(context.Background(), &out); err != nil {
		t.Fatalf("WriteHeaderLine() error = %v", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("WriteHeaderLine() wrote %q, want empty output", got)
	}
}

func TestSplitHeaderSplitsLeadingMetadataLine(t *testing.T) {
	t.Parallel()
	body := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\npayload\n"

	header, rest := SplitHeader(body)

	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222"
	if header != wantHeader {
		t.Fatalf("SplitHeader() header = %q, want %q", header, wantHeader)
	}
	if rest != "payload\n" {
		t.Fatalf("SplitHeader() rest = %q, want %q", rest, "payload\n")
	}
}

func TestSplitHeaderLeavesUnstampedBodyUnchanged(t *testing.T) {
	t.Parallel()
	body := "payload\n"

	header, rest := SplitHeader(body)

	if header != "" {
		t.Fatalf("SplitHeader() header = %q, want empty header", header)
	}
	if rest != body {
		t.Fatalf("SplitHeader() rest = %q, want %q", rest, body)
	}
}

func TestWriteResultRoutesHeaderToErrOut(t *testing.T) {
	t.Parallel()
	corr := correlation.Context{
		TraceID: correlation.TraceID("11111111111111111111111111111111"),
		SpanID:  correlation.SpanID("2222222222222222"),
	}
	ctx := correlation.WithContext(context.Background(), corr)

	var out, errOut bytes.Buffer
	if err := WriteResult(ctx, &out, &errOut, "payload\n"); err != nil {
		t.Fatalf("WriteResult() error = %v", err)
	}
	if got := out.String(); got != "payload\n" {
		t.Fatalf("WriteResult() stdout = %q, want %q", got, "payload\n")
	}
	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\n"
	if got := errOut.String(); got != wantHeader {
		t.Fatalf("WriteResult() stderr = %q, want %q", got, wantHeader)
	}
}

func TestWriteResultWithoutMetadataWritesBodyOnly(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if err := WriteResult(context.Background(), &out, &errOut, "payload\n"); err != nil {
		t.Fatalf("WriteResult() error = %v", err)
	}
	if got := out.String(); got != "payload\n" {
		t.Fatalf("WriteResult() stdout = %q, want %q", got, "payload\n")
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("WriteResult() stderr = %q, want empty", got)
	}
}

func TestWriteResultRoutesStampedHeaderWithoutContext(t *testing.T) {
	t.Parallel()
	body := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\npayload\n"
	var out, errOut bytes.Buffer
	if err := WriteResult(context.Background(), &out, &errOut, body); err != nil {
		t.Fatalf("WriteResult() error = %v", err)
	}
	if got := out.String(); got != "payload\n" {
		t.Fatalf("WriteResult() stdout = %q, want %q", got, "payload\n")
	}
	wantHeader := "🔎 trace_id=11111111111111111111111111111111 span_id=2222222222222222\n"
	if got := errOut.String(); got != wantHeader {
		t.Fatalf("WriteResult() stderr = %q, want %q", got, wantHeader)
	}
}

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
