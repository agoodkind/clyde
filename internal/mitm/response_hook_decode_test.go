package mitm

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"
	"testing"
)

func gzipString(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeResponseBodyForHookGzip(t *testing.T) {
	t.Parallel()
	plain := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Encoding": []string{"gzip"},
			"Content-Type":     []string{"text/event-stream"},
			"Content-Length":   []string{"9999"},
		},
		Body: io.NopCloser(bytes.NewReader(gzipString(t, plain))),
	}
	ok := decodeResponseBodyForHook(resp)
	if !ok {
		t.Fatal("a gzip body must decode")
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("Content-Encoding must be stripped after decoding")
	}
	if resp.Header.Get("Content-Length") != "" {
		t.Fatal("Content-Length must be cleared after decoding")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read decoded body: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("decoded body = %q, want %q", got, plain)
	}
}

func TestDecodeResponseBodyForHookUndecodableEncoding(t *testing.T) {
	t.Parallel()
	raw := []byte("brotli-bytes-the-proxy-cannot-decode")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": []string{"br"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	ok := decodeResponseBodyForHook(resp)
	if ok {
		t.Fatal("an undecodable encoding must report ok=false so the caller skips the transform")
	}
	if resp.Header.Get("Content-Encoding") != "br" {
		t.Fatal("Content-Encoding must be preserved when the body is not decoded")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read preserved body: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("the original body must be preserved when not decoded")
	}
}

func TestDecodeResponseBodyForHookMislabeledGzip(t *testing.T) {
	t.Parallel()
	// Content-Encoding claims gzip but the body is plain text (no gzip magic).
	// The decoder must decline without consuming, and every byte must survive.
	raw := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
		Body:       io.NopCloser(strings.NewReader(raw)),
	}
	ok := decodeResponseBodyForHook(resp)
	if ok {
		t.Fatal("a body whose bytes are not gzip must be declined")
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("Content-Encoding must be preserved when the body is declined")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read preserved body: %v", err)
	}
	if string(got) != raw {
		t.Fatalf("declined body lost bytes: got %q, want %q", got, raw)
	}
}

func TestDecodeResponseBodyForHookPlain(t *testing.T) {
	t.Parallel()
	plain := "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(plain)),
	}
	ok := decodeResponseBodyForHook(resp)
	if !ok {
		t.Fatal("a plain body must be reported as decoded")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("body = %q, want %q", got, plain)
	}
}

type errThenEOFReader struct {
	data []byte
	pos  int
}

func (r *errThenEOFReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (r *errThenEOFReader) Close() error { return nil }

func TestReadAndRewindHookRequestBodyFailsClosedOnReadError(t *testing.T) {
	t.Parallel()
	// A request whose body errors mid-read must not be reseated to the partial
	// buffer; the reseated body must re-surface an error so forwarding fails
	// cleanly rather than sending a truncated request upstream.
	req := &http.Request{Body: &errThenEOFReader{data: []byte("partial-bytes")}}
	got, err := readAndRewindHookRequestBody(req)
	if err == nil {
		t.Fatal("expected an error when the body read fails")
	}
	if string(got) != "partial-bytes" {
		t.Fatalf("returned bytes = %q, want the partial read", got)
	}
	// The reseated body must error on read, not yield the partial bytes.
	if _, readErr := io.ReadAll(req.Body); readErr == nil {
		t.Fatal("reseated request body must re-surface the read error, not forward partial bytes")
	}
	if req.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1 after a read failure", req.ContentLength)
	}
}

func TestResponseHookResponseFromHTTPClearsContentLength(t *testing.T) {
	t.Parallel()
	// A transform is about to rewrite the body, so the seam must hand the
	// transformer an unknown length rather than the stale upstream length.
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Length": []string{"42"}},
		Body:          io.NopCloser(strings.NewReader("body")),
		ContentLength: 42,
	}
	hookResp := responseHookResponseFromHTTP(resp)
	if hookResp.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1", hookResp.ContentLength)
	}
}

func TestDecodeResponseBodyForHookTruncatedGzipMagic(t *testing.T) {
	t.Parallel()
	// A body that matches the gzip magic but EOFs before a full header must be
	// declined without losing the bytes already present, so the caller forwards
	// the (broken) upstream intact rather than a truncated/empty body.
	raw := []byte{0x1f, 0x8b} // gzip magic, then nothing
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	ok := decodeResponseBodyForHook(resp)
	if ok {
		t.Fatal("a truncated gzip header must be declined")
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("Content-Encoding must be preserved when declined")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read preserved body: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("declined body lost bytes: got %v, want %v", got, raw)
	}
}

func zlibString(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	if _, err := writer.Write([]byte(s)); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeResponseBodyForHookDeflate(t *testing.T) {
	t.Parallel()
	plain := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": []string{"deflate"}},
		Body:       io.NopCloser(bytes.NewReader(zlibString(t, plain))),
	}
	if !decodeResponseBodyForHook(resp) {
		t.Fatal("a valid zlib (deflate) body must decode")
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("Content-Encoding must be stripped after decoding")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read decoded body: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("decoded body = %q, want %q", got, plain)
	}
}

func TestDecodeResponseBodyForHookMislabeledDeflate(t *testing.T) {
	t.Parallel()
	// Content-Encoding claims deflate but the bytes are not a valid zlib stream
	// (a raw RFC 1951 deflate or mislabeled body). The decoder must decline
	// without consuming, and every byte must survive for the caller to forward.
	raw := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": []string{"deflate"}},
		Body:       io.NopCloser(strings.NewReader(raw)),
	}
	if decodeResponseBodyForHook(resp) {
		t.Fatal("a body whose bytes are not a valid zlib stream must be declined")
	}
	if resp.Header.Get("Content-Encoding") != "deflate" {
		t.Fatal("Content-Encoding must be preserved when the body is declined")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read preserved body: %v", err)
	}
	if string(got) != raw {
		t.Fatalf("declined body lost bytes: got %q, want %q", got, raw)
	}
}
