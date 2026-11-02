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
	file, resolved, err := openRolloutFile(path)
	if err != nil {
		return nil, err
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

// openRolloutFile opens the requested rollout path and tries its plain or compressed sibling only when the requested file does not exist.
func openRolloutFile(path string) (*os.File, string, error) {
	cleaned := filepath.Clean(path)
	candidates := []string{cleaned}
	if sibling, found := rolloutSiblingPath(cleaned); found {
		candidates = append(candidates, sibling)
	}
	failedPath := cleaned
	var failure error
	for _, candidate := range candidates {
		file, err := os.Open(candidate)
		if err == nil {
			return file, candidate, nil
		}
		missing := errors.Is(err, os.ErrNotExist)
		if failure == nil || !missing {
			failedPath = candidate
			failure = err
		}
		if !missing {
			break
		}
	}
	slog.Warn("codex.store.history.open_failed", "concern", "providers.codex.store", "path", failedPath, "err", failure)
	return nil, "", fmt.Errorf("open codex rollout %s: %w", failedPath, failure)
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
