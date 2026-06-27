package zedstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDecodeThreadJSONSupportsJSONAndZstd(t *testing.T) {
	t.Parallel()

	want := []byte(`{"title":"Thread","version":"0.3.0"}`)

	jsonDecoded, err := DecodeThreadJSON(DataTypeJSON, want)
	if err != nil {
		t.Fatalf("DecodeThreadJSON(json) returned error: %v", err)
	}
	if !bytes.Equal(jsonDecoded, want) {
		t.Fatalf("json decoded = %q, want %q", jsonDecoded, want)
	}

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter returned error: %v", err)
	}
	compressed := encoder.EncodeAll(want, nil)
	if closeErr := encoder.Close(); closeErr != nil {
		t.Fatalf("zstd encoder close: %v", closeErr)
	}

	zstdDecoded, err := DecodeThreadJSON(DataTypeZstd, compressed)
	if err != nil {
		t.Fatalf("DecodeThreadJSON(zstd) returned error: %v", err)
	}
	if !bytes.Equal(zstdDecoded, want) {
		t.Fatalf("zstd decoded = %q, want %q", zstdDecoded, want)
	}
}

func TestDecodeThreadJSONRejectsUnknownDataType(t *testing.T) {
	t.Parallel()

	_, err := DecodeThreadJSON(DataType("binary"), []byte("payload"))
	if err == nil {
		t.Fatal("DecodeThreadJSON returned nil error, want unsupported data type error")
	}
	var typedErr UnknownThreadDataTypeError
	if !errors.As(err, &typedErr) {
		t.Fatalf("error = %T, want UnknownThreadDataTypeError", err)
	}
	if typedErr.DataType != DataType("binary") {
		t.Fatalf("typed error data type = %q, want %q", typedErr.DataType, DataType("binary"))
	}
}
