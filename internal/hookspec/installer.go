package hookspec

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Installer installs registered hooks into user-scoped client settings.
type Installer struct {
	Registry Registry
}

// InstallOptions configures one user-scoped hook installation.
type InstallOptions struct {
	HomeDir  string
	ClydeBin string
	Client   Client
	DryRun   bool
}

// InstallResult describes the settings change produced by one install.
type InstallResult struct {
	SettingsPath string
	Installed    []HookID
	Changed      bool
	DryRun       bool
	Preview      []byte
}

// Install renders every selected hook into the user's settings file.
func (installer Installer) Install(ctx context.Context, options InstallOptions) (InstallResult, error) {
	if err := ctx.Err(); err != nil {
		wrapped := fmt.Errorf("install hooks canceled: %w", err)
		slog.WarnContext(ctx, "hooks install failed", "err", wrapped)
		return InstallResult{}, wrapped
	}
	client := options.Client
	if client == "" {
		client = ClientClaudeCode
	}
	slog.InfoContext(ctx, "hooks install started", "client", client, "dry_run", options.DryRun)
	if client != ClientClaudeCode {
		return InstallResult{}, fmt.Errorf("unsupported hooks client %q", client)
	}
	homeDir, err := readInstallHomeDir(options.HomeDir)
	if err != nil {
		return InstallResult{}, err
	}
	clydeBin, err := readInstallClydeBin(options.ClydeBin)
	if err != nil {
		return InstallResult{}, err
	}
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	existing, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		wrapped := fmt.Errorf("read Claude Code settings %s: %w", settingsPath, err)
		slog.WarnContext(ctx, "hooks install failed", "settings_path", settingsPath, "err", wrapped)
		return InstallResult{}, wrapped
	}

	document, err := unmarshalClaudeSettingsDocument(existing)
	if err != nil {
		return InstallResult{}, err
	}
	hooks := installer.Registry.HooksForClient(client)
	installed := make([]HookID, 0, len(hooks))
	for _, hook := range hooks {
		if err := document.marshalClaudeCodeHookInstall(hook, clydeBin); err != nil {
			return InstallResult{}, err
		}
		installed = append(installed, hook.ID)
	}
	body, err := document.MarshalJSON()
	if err != nil {
		return InstallResult{}, err
	}
	changed := !bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(body))
	result := InstallResult{
		SettingsPath: settingsPath,
		Installed:    installed,
		Changed:      changed,
		DryRun:       options.DryRun,
		Preview:      slices.Clone(body),
	}
	if options.DryRun || !changed {
		slog.InfoContext(
			ctx,
			"hooks install completed",
			"client",
			client,
			"settings_path",
			settingsPath,
			"changed",
			changed,
			"dry_run",
			options.DryRun,
			"installed_count",
			len(installed),
		)
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		wrapped := fmt.Errorf("create Claude Code settings dir: %w", err)
		slog.WarnContext(ctx, "hooks install failed", "settings_path", settingsPath, "err", wrapped)
		return InstallResult{}, wrapped
	}
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		wrapped := fmt.Errorf("write Claude Code settings %s: %w", settingsPath, err)
		slog.WarnContext(ctx, "hooks install failed", "settings_path", settingsPath, "err", wrapped)
		return InstallResult{}, wrapped
	}
	slog.InfoContext(
		ctx,
		"hooks install completed",
		"client",
		client,
		"settings_path",
		settingsPath,
		"changed",
		changed,
		"dry_run",
		options.DryRun,
		"installed_count",
		len(installed),
	)
	return result, nil
}

func readInstallHomeDir(raw string) (string, error) {
	homeDir := strings.TrimSpace(raw)
	if homeDir == "" {
		detected, err := os.UserHomeDir()
		if err != nil {
			wrapped := fmt.Errorf("resolve home dir: %w", err)
			slog.Warn("hooks install option read failed", "err", wrapped)
			return "", wrapped
		}
		homeDir = detected
	}
	if homeDir == "" {
		return "", fmt.Errorf("home dir is required")
	}
	return homeDir, nil
}

func readInstallClydeBin(raw string) (string, error) {
	clydeBin := strings.TrimSpace(raw)
	if clydeBin == "" {
		detected, err := os.Executable()
		if err != nil {
			wrapped := fmt.Errorf("resolve clyde executable: %w", err)
			slog.Warn("hooks install option read failed", "err", wrapped)
			return "", wrapped
		}
		clydeBin = detected
	}
	if clydeBin == "" {
		return "", fmt.Errorf("clyde binary path is required")
	}
	if filepath.IsAbs(clydeBin) {
		return clydeBin, nil
	}
	absolute, err := filepath.Abs(clydeBin)
	if err != nil {
		wrapped := fmt.Errorf("resolve clyde binary path %q: %w", clydeBin, err)
		slog.Warn("hooks install option read failed", "err", wrapped)
		return "", wrapped
	}
	return absolute, nil
}
