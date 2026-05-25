package util

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	DefaultFileMode = 0o644
	DefaultDirMode  = 0o755
)

func ensureDirForFile(filePath string) error {
	return EnsureDir(filepath.Dir(filePath))
}

// EnsureDir creates a directory and all parent directories if they don't exist.
// Returns an error if creation fails.
func EnsureDir(path string) error {
	return os.MkdirAll(path, DefaultDirMode)
}

// FileExists checks if a file exists at the given path.
// Returns true if the file exists, false otherwise.
func FileExists(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

// DirExists checks if a directory exists at the given path.
// Returns true if the directory exists, false otherwise.
func DirExists(path string) bool {
	pathInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	return pathInfo.IsDir()
}

// ReadJSON reads a JSON file and unmarshals it into the provided value.
// Returns an error if reading or unmarshaling fails.
func ReadJSON[T any](path string, v *T) error {
	data, err := ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// WriteJSON marshals the provided value to JSON and writes it to a file.
// Creates parent directories if they don't exist.
// Uses indented formatting for readability.
// Returns an error if marshaling or writing fails.
func WriteJSON[T any](path string, v T) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, data)
}

// HomeDir returns the user's home directory path.
// Returns an error if the home directory cannot be determined.
func HomeDir() (string, error) {
	return os.UserHomeDir()
}

// WriteFile writes content to a file, creating parent directories if
// needed. The write is atomic from the reader's perspective: the
// content lands in a sibling temp file with a unique suffix produced
// by [os.CreateTemp] and is then renamed onto the destination. The
// per-writer unique suffix means concurrent writers to the same path
// no longer collide on a fixed ".tmp" name, so the rename step does
// not race itself into ENOENT. Each failure path emits one structured
// warn event with the failing step labeled so callers can correlate the
// returned wrapped error with the lifecycle log.
func WriteFile(path string, content []byte) error {
	if err := ensureDirForFile(path); err != nil {
		slog.Warn("util.write_file.failed", "path", path, "step", "ensure_dir", "err", err)
		return fmt.Errorf("ensure dir for %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		slog.Warn("util.write_file.failed", "path", path, "step", "create_temp", "err", err)
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		slog.Warn("util.write_file.failed", "path", path, "step", "write_temp", "tmp", tmpName, "err", err)
		return fmt.Errorf("write temp %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(DefaultFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		slog.Warn("util.write_file.failed", "path", path, "step", "chmod_temp", "tmp", tmpName, "err", err)
		return fmt.Errorf("chmod temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		slog.Warn("util.write_file.failed", "path", path, "step", "close_temp", "tmp", tmpName, "err", err)
		return fmt.Errorf("close temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		slog.Warn("util.write_file.failed", "path", path, "step", "rename", "tmp", tmpName, "err", err)
		return fmt.Errorf("rename temp %s onto %s: %w", tmpName, path, err)
	}
	return nil
}

// ReadFile reads the entire contents of a file.
// Returns the file contents and any error encountered.
func ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// RemoveAll removes a path and all its contents.
// Returns an error if removal fails.
func RemoveAll(path string) error {
	return os.RemoveAll(path)
}
