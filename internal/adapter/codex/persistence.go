package codex

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

func writeCodexAuthFileAtomic(
	ctx context.Context,
	log *slog.Logger,
	path string,
	data []byte,
) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".clyde-codex-auth-*")
	if err != nil {
		log.WarnContext(
			ctx,
			"adapter.codex.auth.tempfile_failed",
			"concern",
			"adapter.providers.codex.request",
			"component",
			"adapter",
			"subcomponent",
			"codex",
			"dir",
			dir,
			"err",
			err.Error(),
		)

		return fmt.Errorf("create temp codex auth file: %w", err)
	}

	tmpName := tmp.Name()
	if writeErr := writeTempAuthFile(tmp, data); writeErr != nil {
		_ = os.Remove(tmpName)
		log.WarnContext(
			ctx,
			"adapter.codex.auth.tempfile_write_failed",
			"concern",
			"adapter.providers.codex.request",
			"component",
			"adapter",
			"subcomponent",
			"codex",
			"path",
			tmpName,
			"err",
			writeErr.Error(),
		)

		return writeErr
	}

	if renameErr := os.Rename(tmpName, path); renameErr != nil {
		_ = os.Remove(tmpName)
		log.WarnContext(
			ctx,
			"adapter.codex.auth.rename_failed",
			"concern",
			"adapter.providers.codex.request",
			"component",
			"adapter",
			"subcomponent",
			"codex",
			"src",
			tmpName,
			"dst",
			path,
			"err",
			renameErr.Error(),
		)

		return fmt.Errorf("rename codex auth file: %w", renameErr)
	}

	log.InfoContext(
		ctx,
		"adapter.codex.auth.file_written",
		"concern",
		"adapter.providers.codex.request",
		"component",
		"adapter",
		"subcomponent",
		"codex",
		"path",
		path,
		"bytes",
		len(data),
	)

	return nil
}

func writeTempAuthFile(tmp *os.File, data []byte) error {
	log := slog.Default()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		log.Warn(
			"adapter.codex.auth.temp_write_failed",
			"concern",
			"adapter.providers.codex.request",
			"component",
			"adapter",
			"subcomponent",
			"codex",
			"err",
			err.Error(),
		)

		return fmt.Errorf("write temp codex auth file: %w", err)
	}

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		log.Warn(
			"adapter.codex.auth.temp_chmod_failed",
			"concern",
			"adapter.providers.codex.request",
			"component",
			"adapter",
			"subcomponent",
			"codex",
			"err",
			err.Error(),
		)

		return fmt.Errorf("chmod temp codex auth file: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		log.Warn(
			"adapter.codex.auth.temp_sync_failed",
			"concern",
			"adapter.providers.codex.request",
			"component",
			"adapter",
			"subcomponent",
			"codex",
			"err",
			err.Error(),
		)

		return fmt.Errorf("sync temp codex auth file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		log.Warn(
			"adapter.codex.auth.temp_close_failed",
			"concern",
			"adapter.providers.codex.request",
			"component",
			"adapter",
			"subcomponent",
			"codex",
			"err",
			err.Error(),
		)

		return fmt.Errorf("close temp codex auth file: %w", err)
	}

	return nil
}
