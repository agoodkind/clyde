package antigravity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	antigravityInfoPlist = "/Applications/Antigravity.app/Contents/Info.plist"
	clientVersionTimeout = 5 * time.Second
)

var (
	errClientVersionRequired = errors.New(
		"installed Antigravity app is missing. Set adapter.antigravity.client_version",
	)
	errInstalledVersionEmpty = errors.New("installed Antigravity app version is empty")
)

// ResolveClientVersion reads the installed Antigravity version.
//
// It uses the configured version only when the app is absent.
//
//	version, err := ResolveClientVersion(configuredVersion)
func ResolveClientVersion(fallback string) (string, error) {
	return resolveClientVersion(antigravityInfoPlist, fallback, readPlistVersion)
}

func resolveClientVersion(
	path string,
	fallback string,
	readVersion func(string) ([]byte, error),
) (string, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if strings.TrimSpace(fallback) != "" {
			return strings.TrimSpace(fallback), nil
		}

		return "", errClientVersionRequired
	}

	if err != nil {
		slog.Warn("adapter.antigravity.version_stat_failed", "path", path, "err", err)

		return "", fmt.Errorf("inspect Antigravity app version file: %w", err)
	}

	output, err := readVersion(path)
	if err != nil {
		return "", fmt.Errorf("read Antigravity app version: %w", err)
	}

	version := strings.TrimSpace(string(output))
	if version == "" {
		return "", errInstalledVersionEmpty
	}

	return version, nil
}

func readPlistVersion(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clientVersionTimeout)
	defer cancel()

	output, err := exec.CommandContext(
		ctx,
		"/usr/bin/plutil",
		"-extract",
		"CFBundleShortVersionString",
		"raw",
		path,
	).Output()
	if err != nil {
		slog.Warn("adapter.antigravity.read_version_failed", "path", path, "err", err)

		return nil, fmt.Errorf("read CFBundleShortVersionString: %w", err)
	}

	return output, nil
}
