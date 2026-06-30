package mitm

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstdBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()
	return enc.EncodeAll(payload, nil)
}

func TestDecodeForCaptureZstd(t *testing.T) {
	want := []byte(`{"alpha":"beta","gamma":42}`)
	got, decoded := decodeForCapture(zstdBytes(t, want), "zstd")
	if !decoded {
		t.Fatal("expected decoded=true")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func gzipBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zlibBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

func flateBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeForCaptureGzip(t *testing.T) {
	want := []byte(`{"foo":"bar"}`)
	got, decoded := decodeForCapture(gzipBytes(t, want), "gzip")
	if !decoded {
		t.Fatal("expected decoded=true")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDecodeForCaptureZlib(t *testing.T) {
	want := []byte(`{"alpha":1}`)
	got, decoded := decodeForCapture(zlibBytes(t, want), "deflate")
	if !decoded {
		t.Fatal("expected decoded=true")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDecodeForCaptureRawDeflate(t *testing.T) {
	want := []byte(`raw rfc 1951 deflate body`)
	got, decoded := decodeForCapture(flateBytes(t, want), "deflate")
	if !decoded {
		t.Fatal("expected decoded=true")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDecodeForCaptureUnknownEncodingReturnsRaw(t *testing.T) {
	raw := []byte("raw-body")
	got, decoded := decodeForCapture(raw, "br")
	if decoded {
		t.Fatal("expected decoded=false for brotli")
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q want %q", got, raw)
	}
}

func TestDecodeForCaptureNoEncodingReturnsRaw(t *testing.T) {
	raw := []byte(`{"a":1}`)
	got, decoded := decodeForCapture(raw, "")
	if decoded {
		t.Fatal("expected decoded=false")
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q want %q", got, raw)
	}
}

func TestDecodeForCaptureMalformedFallsBackToRaw(t *testing.T) {
	raw := []byte("not actually gzip")
	got, decoded := decodeForCapture(raw, "gzip")
	if decoded {
		t.Fatal("expected decoded=false on malformed input")
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q want %q", got, raw)
	}
}
