// Package testsupport creates Codex rollout fixtures for provider and integration tests.
package testsupport

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// CompressRollout encodes body as one Zstandard frame and fails the test if encoding fails.
func CompressRollout(tb testing.TB, body string) []byte {
	tb.Helper()
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed, zstd.WithEncoderConcurrency(1))
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := encoder.Write([]byte(body)); err != nil {
		tb.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		tb.Fatal(err)
	}
	return compressed.Bytes()
}
