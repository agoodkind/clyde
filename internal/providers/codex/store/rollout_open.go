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

const (
	rolloutFilenamePrefix   = "rollout-"
	rolloutPlainSuffix      = ".jsonl"
	rolloutCompressedSuffix = ".jsonl.zst"
)

// compressedRollout pairs a zstd decoder with its source file.
type compressedRollout struct {
	*zstd.Decoder
	file *os.File
}

// Close closes the zstd decoder before closing the source file.
func (rollout compressedRollout) Close() error {
	rollout.Decoder.Close()
	if err := rollout.file.Close(); err != nil {
		slog.Warn("codex.store.history.zstd_close_failed", "concern", "providers.codex.store", "path", rollout.file.Name(), "err", err)
		return fmt.Errorf("close compressed codex rollout: %w", err)
	}
	return nil
}

// openRollout opens a rollout for reading and decodes zstd compression when needed.
func openRollout(path string) (io.ReadCloser, error) {
	resolved := resolveRolloutPath(path)
	file, err := os.Open(resolved)
	if err != nil {
		slog.Warn("codex.store.history.open_failed", "concern", "providers.codex.store", "path", path, "err", err)
		return nil, fmt.Errorf("open codex rollout %s: %w", path, err)
	}
	if !strings.HasSuffix(resolved, rolloutCompressedSuffix) {
		return file, nil
	}
	decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		slog.Warn("codex.store.history.zstd_open_failed", "concern", "providers.codex.store", "path", resolved, "err", err)
		return nil, errors.Join(fmt.Errorf("open compressed codex rollout %s: %w", resolved, err), file.Close())
	}
	return compressedRollout{Decoder: decoder, file: file}, nil
}

// resolveRolloutPath returns the cleaned path, substituting an existing plain or compressed sibling only when the requested path does not exist.
func resolveRolloutPath(path string) string {
	cleaned := filepath.Clean(path)
	if _, err := os.Stat(cleaned); !errors.Is(err, os.ErrNotExist) {
		return cleaned
	}
	sibling, found := rolloutSiblingPath(cleaned)
	if !found {
		return cleaned
	}
	if _, err := os.Stat(sibling); err != nil {
		return cleaned
	}
	return sibling
}

// rolloutSiblingPath returns the corresponding plain or compressed path and reports whether the input has a supported rollout suffix.
func rolloutSiblingPath(path string) (string, bool) {
	if plain, compressed := strings.CutSuffix(path, rolloutCompressedSuffix); compressed {
		return plain + rolloutPlainSuffix, true
	}
	if plain, isPlain := strings.CutSuffix(path, rolloutPlainSuffix); isPlain {
		return plain + rolloutCompressedSuffix, true
	}
	return "", false
}

// rolloutFilenameStem extracts the filename stem only when the rollout prefix and a supported suffix match.
func rolloutFilenameStem(name string) (string, bool) {
	stem, prefixed := strings.CutPrefix(name, rolloutFilenamePrefix)
	if !prefixed {
		return "", false
	}
	if base, compressed := strings.CutSuffix(stem, rolloutCompressedSuffix); compressed {
		return base, true
	}
	return strings.CutSuffix(stem, rolloutPlainSuffix)
}

// shadowedByPlainRollout reports whether a compressed rollout has an existing plain sibling.
func shadowedByPlainRollout(path string) bool {
	plain, compressed := strings.CutSuffix(path, rolloutCompressedSuffix)
	if !compressed {
		return false
	}
	_, err := os.Stat(plain + rolloutPlainSuffix)
	return err == nil
}
