// Command staticembed-model prepares the model files for compilation.
// It verifies cached files against the manifest and downloads replacements
// from the pinned revision. A replacement must pass SHA-256 verification
// before the command renames it into place.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	modelDirectory  = "internal/conversation/staticembed/model"
	manifestName    = "manifest.json"
	downloadTimeout = 10 * time.Minute
	fileMode        = 0o644
	urlFormat       = "https://huggingface.co/%s/resolve/%s/%s"
)

type pinnedFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type manifest struct {
	Repository string       `json:"repository"`
	Revision   string       `json:"revision"`
	License    string       `json:"license"`
	Files      []pinnedFile `json:"files"`
}

var errHashMismatch = errors.New("SHA-256 does not match the manifest")

func main() {
	slog.Info("staticembed_model.started", "component", "staticembed-model", "directory", modelDirectory)
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.ErrorContext(ctx, "staticembed_model.failed", "component", "staticembed-model", "err", err)
		return 1
	}
	return 0
}

func run(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(modelDirectory, manifestName))
	if err != nil {
		slog.ErrorContext(ctx, "staticembed_model.manifest_read_failed", "component", "staticembed-model", "err", err)
		return fmt.Errorf("read the model manifest: %w", err)
	}
	var pinned manifest
	if err := json.Unmarshal(raw, &pinned); err != nil {
		slog.ErrorContext(ctx, "staticembed_model.manifest_parse_failed", "component", "staticembed-model", "err", err)
		return fmt.Errorf("parse the model manifest: %w", err)
	}
	if pinned.Repository == "" || pinned.Revision == "" || len(pinned.Files) == 0 {
		return errors.New("the model manifest needs a repository, a revision, and at least one file")
	}
	client := &http.Client{Timeout: downloadTimeout}
	for _, file := range pinned.Files {
		if err := ensureFile(ctx, client, pinned, file); err != nil {
			return err
		}
	}
	return nil
}

func ensureFile(ctx context.Context, client *http.Client, pinned manifest, file pinnedFile) error {
	path := filepath.Join(modelDirectory, file.Name)
	sum, err := fileSHA256(path)
	if err == nil && sum == file.SHA256 {
		slog.InfoContext(ctx, "staticembed_model.cached", "component", "staticembed-model", "file", file.Name)
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	url := fmt.Sprintf(urlFormat, pinned.Repository, pinned.Revision, file.Name)
	slog.InfoContext(ctx, "staticembed_model.downloading", "component", "staticembed-model", "file", file.Name, "url", url)
	temporary, err := download(ctx, client, url, path)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary) }()
	downloaded, err := fileSHA256(temporary)
	if err != nil {
		return err
	}
	if downloaded != file.SHA256 {
		err := fmt.Errorf("%s from %s: %w: got %s, want %s", file.Name, url, errHashMismatch, downloaded, file.SHA256)
		slog.ErrorContext(ctx, "staticembed_model.hash_mismatch", "component", "staticembed-model", "err", err)
		return err
	}
	if err := os.Chmod(temporary, fileMode); err != nil {
		slog.ErrorContext(ctx, "staticembed_model.chmod_failed", "component", "staticembed-model", "file", file.Name, "err", err)
		return fmt.Errorf("set the mode of %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		slog.ErrorContext(ctx, "staticembed_model.rename_failed", "component", "staticembed-model", "file", file.Name, "err", err)
		return fmt.Errorf("move %s into place: %w", file.Name, err)
	}
	return nil
}

func download(ctx context.Context, client *http.Client, url string, path string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.ErrorContext(ctx, "staticembed_model.request_failed", "component", "staticembed-model", "url", url, "err", err)
		return "", fmt.Errorf("build the request for %s: %w", url, err)
	}
	response, err := client.Do(request)
	if err != nil {
		slog.ErrorContext(ctx, "staticembed_model.download_failed", "component", "staticembed-model", "url", url, "err", err)
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP status %s", url, response.Status)
	}
	output, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".download-*")
	if err != nil {
		slog.ErrorContext(ctx, "staticembed_model.temp_failed", "component", "staticembed-model", "path", path, "err", err)
		return "", fmt.Errorf("create a temporary file for %s: %w", path, err)
	}
	_, copyErr := io.Copy(output, response.Body)
	closeErr := output.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(output.Name())
		slog.ErrorContext(ctx, "staticembed_model.write_failed", "component", "staticembed-model", "url", url, "err", err)
		return "", fmt.Errorf("write the download of %s: %w", url, err)
	}
	return output.Name(), nil
}

// A missing file triggers a download rather than a warning.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("open %s: %w", path, os.ErrNotExist)
	}
	if err != nil {
		slog.Warn("staticembed_model.open_failed", "component", "staticembed-model", "path", path, "err", err)
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		slog.Warn("staticembed_model.hash_read_failed", "component", "staticembed-model", "path", path, "err", err)
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
