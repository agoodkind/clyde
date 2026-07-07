package mitm

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// mitmContentEncoding enumerates the Content-Encoding values the MITM
// proxy's capture decoder recognizes.
type mitmContentEncoding string

const (
	mitmContentEncodingGzip      mitmContentEncoding = "gzip"
	mitmContentEncodingXGzip     mitmContentEncoding = "x-gzip"
	mitmContentEncodingDeflate   mitmContentEncoding = "deflate"
	mitmContentEncodingZstd      mitmContentEncoding = "zstd"
	mitmContentEncodingZstandard mitmContentEncoding = "zstandard"
)

// decodeForCapture transparently decompresses captured response
// bytes when a Content-Encoding the standard library can handle is
// present. Forward bytes to the client are unaffected. Returns the
// original bytes when the encoding is unknown (zstd, brotli) or when
// decompression fails. The boolean return reports whether
// decompression actually ran.
func decodeForCapture(raw []byte, contentEncoding string) ([]byte, bool) {
	enc := strings.TrimSpace(strings.ToLower(contentEncoding))
	if enc == "" || enc == "identity" {
		return raw, false
	}
	switch mitmContentEncoding(enc) {
	case mitmContentEncodingGzip, mitmContentEncodingXGzip:
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, false
		}
		defer func() { _ = gr.Close() }()
		out, err := io.ReadAll(gr)
		if err != nil {
			return raw, false
		}
		return out, true
	case mitmContentEncodingDeflate:
		// Some servers send raw RFC 1951 deflate; others send RFC 1950 zlib.
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err == nil {
			defer func() { _ = zr.Close() }()
			out, rerr := io.ReadAll(zr)
			if rerr == nil {
				return out, true
			}
		}
		fr := flate.NewReader(bytes.NewReader(raw))
		defer func() { _ = fr.Close() }()
		out, err := io.ReadAll(fr)
		if err != nil {
			return raw, false
		}
		return out, true
	case mitmContentEncodingZstd, mitmContentEncodingZstandard:
		dec, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, false
		}
		defer func() { dec.Close() }()
		out, err := io.ReadAll(dec)
		if err != nil {
			return raw, false
		}
		return out, true
	}
	return raw, false
}

// decompressingReadCloser streams a decompressed body and, on Close, closes both
// the decompressor and the underlying source.
type decompressingReadCloser struct {
	reader  io.Reader
	closers []io.Closer
}

func (d *decompressingReadCloser) Read(p []byte) (int, error) {
	n, err := d.reader.Read(p)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		return n, io.EOF
	default:
		return n, fmt.Errorf("read decompressed response body: %w", err)
	}
}

func (d *decompressingReadCloser) Close() error {
	var firstErr error
	for _, closer := range d.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// bufferedReadCloser reads through a [bufio.Reader] while closing the underlying
// source. It re-seats a body a decoder declined so bytes buffered during magic
// detection are never lost.
type bufferedReadCloser struct {
	reader *bufio.Reader
	closer io.Closer
}

func (b *bufferedReadCloser) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		return n, io.EOF
	default:
		return n, fmt.Errorf("read buffered response body: %w", err)
	}
}

func (b *bufferedReadCloser) Close() error {
	if b.closer == nil {
		return nil
	}
	if err := b.closer.Close(); err != nil {
		slog.Warn("mitm.decompress.buffered_body_close_failed", "concern", "providers.mitm.wire", "err", err)
		return fmt.Errorf("close buffered response body: %w", err)
	}
	return nil
}

// zstdMagic identifies a zstd stream by its leading bytes so the decoder is only
// constructed for a body that actually carries that encoding.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// gzipHeaderPeekBytes bounds how many leading bytes are peeked to validate a gzip
// header. A gzip header is 10 bytes plus optional name/comment/extra fields; a
// server response carries none, so this window comfortably covers it.
const gzipHeaderPeekBytes = 512

func peekHasPrefix(buffered *bufio.Reader, magic []byte) bool {
	head, err := buffered.Peek(len(magic))
	if err != nil {
		return false
	}
	return bytes.Equal(head, magic)
}

// gzipHeaderDecodable reports whether the buffered stream begins with a complete,
// valid gzip header. It trial-constructs a reader over a peeked copy of the
// leading bytes; a peek does not consume, so a body that matches the gzip magic
// but is truncated before a full header is declined without losing bytes (the
// caller then forwards it intact), where constructing [gzip.NewReader] directly
// on the buffered reader would consume the partial header first.
func gzipHeaderDecodable(buffered *bufio.Reader) bool {
	head, _ := buffered.Peek(gzipHeaderPeekBytes)
	if len(head) < 2 {
		return false
	}
	gr, err := gzip.NewReader(bytes.NewReader(head))
	if err != nil {
		return false
	}
	_ = gr.Close()
	return true
}

// zlibHeaderPeekBytes bounds how many leading bytes are peeked to validate a zlib
// header. The header is 2 bytes, plus a 4-byte dictionary id when FDICT is set,
// so this window covers both without reading compressed data.
const zlibHeaderPeekBytes = 8

// zlibHeaderDecodable reports whether the buffered stream begins with a complete,
// valid RFC 1950 zlib header. It trial-constructs a reader over a peeked copy of
// the leading bytes; a peek does not consume, so a body truncated before a full
// header (or with FDICT set but no dictionary) is declined without losing bytes,
// where constructing [zlib.NewReader] directly on the buffered reader would
// consume the partial header first. A raw RFC 1951 deflate stream or a mislabeled
// body fails the header validation and is declined.
func zlibHeaderDecodable(buffered *bufio.Reader) bool {
	head, _ := buffered.Peek(zlibHeaderPeekBytes)
	if len(head) < 2 {
		return false
	}
	zr, err := zlib.NewReader(bytes.NewReader(head))
	if err != nil {
		return false
	}
	_ = zr.Close()
	return true
}

// newDecompressingReadCloser wraps buffered in a streaming decompressor for a
// Content-Encoding the standard library and zstd can handle, so a caller decodes
// a body without buffering it whole. It peeks the encoding's magic bytes first
// (a peek does not consume), so a body whose declared encoding does not match its
// bytes is declined without any read, and the caller forwards the original body
// intact. Only the deflate RFC 1950 zlib form is hook-transformable: a raw RFC
// 1951 deflate stream has no header to magic-detect, so it is declined and
// forwarded untouched rather than risk mis-decoding (the capture-only
// decodeForCapture path, which never rewrites the client stream, decodes it by
// trial instead). An unknown or unsupported encoding (brotli) is also declined.
// underlying is closed alongside the decoder.
func newDecompressingReadCloser(buffered *bufio.Reader, underlying io.Closer, contentEncoding string) (io.ReadCloser, bool) {
	enc := strings.TrimSpace(strings.ToLower(contentEncoding))
	switch mitmContentEncoding(enc) {
	case mitmContentEncodingGzip, mitmContentEncodingXGzip:
		if !gzipHeaderDecodable(buffered) {
			return nil, false
		}
		gr, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, false
		}
		return &decompressingReadCloser{reader: gr, closers: []io.Closer{gr, underlying}}, true
	case mitmContentEncodingDeflate:
		if !zlibHeaderDecodable(buffered) {
			return nil, false
		}
		zr, err := zlib.NewReader(buffered)
		if err != nil {
			return nil, false
		}
		return &decompressingReadCloser{reader: zr, closers: []io.Closer{zr, underlying}}, true
	case mitmContentEncodingZstd, mitmContentEncodingZstandard:
		if !peekHasPrefix(buffered, zstdMagic) {
			return nil, false
		}
		dec, err := zstd.NewReader(buffered)
		if err != nil {
			return nil, false
		}
		readCloser := dec.IOReadCloser()
		return &decompressingReadCloser{reader: readCloser, closers: []io.Closer{readCloser, underlying}}, true
	}
	return nil, false
}
