package codexstore

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const compressedRolloutSuffix = ".zst"

// rolloutReader closes both the decoder and its source file, including when a
// stream consumer stops before the end of the rollout.
type rolloutReader struct {
	io.Reader
	file    *os.File
	decoder *zstd.Decoder
}

func (reader *rolloutReader) Close() error {
	if reader.decoder != nil {
		reader.decoder.Close()
	}
	if err := reader.file.Close(); err != nil {
		slog.Warn("codex.store.history.close_failed", "concern", "providers.codex.store", "path", reader.file.Name(), "err", err)
		return fmt.Errorf("close codex rollout %s: %w", reader.file.Name(), err)
	}
	return nil
}

// openRollout prefers a plain sibling and resolves paths cached before Codex
// compressed or materialized a rollout. Decoding reads directly from the file.
func openRollout(path string) (io.ReadCloser, error) {
	path = filepath.Clean(path)
	plainPath := strings.TrimSuffix(path, compressedRolloutSuffix)
	file, err := os.Open(plainPath)
	if err == nil {
		return file, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("open codex rollout %s: %w", plainPath, err)
	}
	if !strings.HasSuffix(plainPath, ".jsonl") {
		return nil, fmt.Errorf("open codex rollout %s: %w", plainPath, err)
	}
	compressedPath := plainPath + compressedRolloutSuffix
	file, err = os.Open(compressedPath)
	if err != nil {
		return nil, fmt.Errorf("open codex rollout %s: %w", compressedPath, err)
	}
	decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		slog.Warn("codex.store.history.decoder_failed", "concern", "providers.codex.store", "path", compressedPath, "err", err)
		return nil, errors.Join(fmt.Errorf("initialize codex rollout decoder %s: %w", compressedPath, err), file.Close())
	}
	return &rolloutReader{Reader: decoder, file: file, decoder: decoder}, nil
}
