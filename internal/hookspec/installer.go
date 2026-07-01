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

// InstallFileResult describes one settings file produced by an install.
type InstallFileResult struct {
	Client       Client
	SettingsPath string
	Installed    []HookID
	Changed      bool
	Preview      []byte
}

// InstallResult describes the settings changes produced by one install.
type InstallResult struct {
	Files   []InstallFileResult
	Changed bool
	DryRun  bool
}

// Install renders every selected hook into the user's settings files.
func (installer Installer) Install(ctx context.Context, options InstallOptions) (InstallResult, error) {
	if err := ctx.Err(); err != nil {
		wrapped := fmt.Errorf("install hooks canceled: %w", err)
		slog.WarnContext(ctx, "hooks install failed", "err", wrapped)
		return InstallResult{}, wrapped
	}
	client := options.Client
	if client == "" {
		client = ClientAll
	}
	clients, err := installClients(client)
	if err != nil {
		return InstallResult{}, err
	}
	homeDir, err := readInstallHomeDir(options.HomeDir)
	if err != nil {
		return InstallResult{}, err
	}
	clydeBin, err := readInstallClydeBin(options.ClydeBin)
	if err != nil {
		return InstallResult{}, err
	}
	slog.InfoContext(ctx, "hooks install started", "client", client, "dry_run", options.DryRun)

	result := InstallResult{
		Files:   make([]InstallFileResult, 0, len(clients)),
		Changed: false,
		DryRun:  options.DryRun,
	}
	for _, selected := range clients {
		fileResult, err := installer.installClient(ctx, selected, homeDir, clydeBin, options.DryRun)
		if err != nil {
			return InstallResult{}, err
		}
		result.Changed = result.Changed || fileResult.Changed
		result.Files = append(result.Files, fileResult)
	}
	slog.InfoContext(
		ctx,
		"hooks install completed",
		"client",
		client,
		"changed",
		result.Changed,
		"dry_run",
		options.DryRun,
		"settings_count",
		len(result.Files),
	)
	return result, nil
}

func installClients(client Client) ([]Client, error) {
	switch client {
	case ClientAll:
		return SupportedClients(), nil
	case ClientClaudeCode, ClientCodex, ClientCursor:
		return []Client{client}, nil
	default:
		return nil, fmt.Errorf("unsupported hooks client %q", client)
	}
}

func (installer Installer) installClient(ctx context.Context, client Client, homeDir string, clydeBin string, dryRun bool) (InstallFileResult, error) {
	installs := installer.Registry.InstallsForClient(client)
	switch client {
	case ClientClaudeCode:
		return installer.installClaudeCode(ctx, homeDir, clydeBin, installs, dryRun)
	case ClientCodex:
		return installer.installCodex(ctx, homeDir, clydeBin, installs, dryRun)
	case ClientCursor:
		return installer.installCursor(ctx, homeDir, clydeBin, installs, dryRun)
	case ClientAll:
		return InstallFileResult{}, fmt.Errorf("install client must be concrete, got %q", client)
	default:
		return InstallFileResult{}, fmt.Errorf("unsupported hooks client %q", client)
	}
}

func (installer Installer) installClaudeCode(ctx context.Context, homeDir string, clydeBin string, installs []RegisteredInstall, dryRun bool) (InstallFileResult, error) {
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	existing, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		wrapped := fmt.Errorf("read Claude Code settings %s: %w", settingsPath, err)
		slog.WarnContext(ctx, "hooks install failed", "settings_path", settingsPath, "err", wrapped)
		return InstallFileResult{}, wrapped
	}
	document, err := unmarshalClaudeSettingsDocument(existing)
	if err != nil {
		return InstallFileResult{}, err
	}
	if err := document.marshalClaudeCodeHookInstalls(installs, installer.Registry.ClydeCommandSignatures(), clydeBin); err != nil {
		return InstallFileResult{}, err
	}
	body, err := document.MarshalJSON()
	if err != nil {
		return InstallFileResult{}, err
	}
	return writeInstallFile(ctx, ClientClaudeCode, settingsPath, existing, body, installs, dryRun)
}

func (installer Installer) installCodex(ctx context.Context, homeDir string, clydeBin string, installs []RegisteredInstall, dryRun bool) (InstallFileResult, error) {
	settingsPath := filepath.Join(homeDir, ".codex", "config.toml")
	existing, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		wrapped := fmt.Errorf("read Codex settings %s: %w", settingsPath, err)
		slog.WarnContext(ctx, "hooks install failed", "settings_path", settingsPath, "err", wrapped)
		return InstallFileResult{}, wrapped
	}
	body, err := marshalCodexHookInstalls(existing, installs, installer.Registry.ClydeCommandSignatures(), clydeBin, settingsPath)
	if err != nil {
		return InstallFileResult{}, err
	}
	return writeInstallFile(ctx, ClientCodex, settingsPath, existing, body, installs, dryRun)
}

func (installer Installer) installCursor(ctx context.Context, homeDir string, clydeBin string, installs []RegisteredInstall, dryRun bool) (InstallFileResult, error) {
	settingsPath := filepath.Join(homeDir, ".cursor", "hooks.json")
	existing, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		wrapped := fmt.Errorf("read Cursor hooks %s: %w", settingsPath, err)
		slog.WarnContext(ctx, "hooks install failed", "settings_path", settingsPath, "err", wrapped)
		return InstallFileResult{}, wrapped
	}
	document, err := unmarshalCursorHooksDocument(existing)
	if err != nil {
		return InstallFileResult{}, err
	}
	if err := document.marshalCursorHookInstalls(installs, installer.Registry.ClydeCommandSignatures(), clydeBin); err != nil {
		return InstallFileResult{}, err
	}
	body, err := document.MarshalJSON()
	if err != nil {
		return InstallFileResult{}, err
	}
	return writeInstallFile(ctx, ClientCursor, settingsPath, existing, body, installs, dryRun)
}

func writeInstallFile(ctx context.Context, client Client, path string, existing []byte, body []byte, installs []RegisteredInstall, dryRun bool) (InstallFileResult, error) {
	changed := !bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(body))
	result := InstallFileResult{
		Client:       client,
		SettingsPath: path,
		Installed:    installIDs(installs),
		Changed:      changed,
		Preview:      slices.Clone(body),
	}
	if dryRun || !changed {
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		wrapped := fmt.Errorf("create hooks settings dir %s: %w", filepath.Dir(path), err)
		slog.WarnContext(ctx, "hooks install failed", "settings_path", path, "err", wrapped)
		return InstallFileResult{}, wrapped
	}
	// #nosec G703 -- path is a Clyde-generated user settings path selected by
	// installClient after resolving the user's home directory.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		wrapped := fmt.Errorf("write hooks settings %s: %w", path, err)
		slog.WarnContext(ctx, "hooks install failed", "settings_path", path, "err", wrapped)
		return InstallFileResult{}, wrapped
	}
	return result, nil
}

func installIDs(installs []RegisteredInstall) []HookID {
	ids := make([]HookID, 0, len(installs))
	for _, install := range installs {
		if slices.Contains(ids, install.HookID) {
			continue
		}
		ids = append(ids, install.HookID)
	}
	return ids
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
