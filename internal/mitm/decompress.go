package mitm

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
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
