package hookspec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// UninstallOptions configures one user-scoped hook removal.
type UninstallOptions struct {
	HomeDir string
}

// UninstallFileResult describes one hook settings file.
type UninstallFileResult struct {
	Client       Client
	SettingsPath string
	Changed      bool
}

// UninstallResult describes all hook settings changes.
type UninstallResult struct {
	Files   []UninstallFileResult
	Changed bool
}

// Uninstaller removes Clyde hook commands without changing unrelated hooks.
type Uninstaller struct {
	Registry Registry
}

// Uninstall removes Clyde hook commands from every supported client.
func (uninstaller Uninstaller) Uninstall(ctx context.Context, options UninstallOptions) (UninstallResult, error) {
	slog.InfoContext(ctx, "hooks.uninstall_started", "component", "hookspec")
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "hooks.uninstall_failed", "component", "hookspec", "err", err)
		return UninstallResult{}, fmt.Errorf("uninstall hooks canceled: %w", err)
	}
	homeDir, err := readInstallHomeDir(options.HomeDir)
	if err != nil {
		slog.WarnContext(ctx, "hooks.uninstall_failed", "component", "hookspec", "err", err)
		return UninstallResult{}, err
	}
	signatures := uninstaller.Registry.managedCommandSignatures()
	knownCommands := managedClydeExecutables(homeDir)
	targets := []struct {
		client Client
		path   string
	}{
		{client: ClientClaudeCode, path: filepath.Join(homeDir, ".claude", "settings.json")},
		{client: ClientCodex, path: filepath.Join(homeDir, ".codex", "config.toml")},
		{client: ClientCursor, path: filepath.Join(homeDir, ".cursor", "hooks.json")},
	}
	result := UninstallResult{Files: make([]UninstallFileResult, 0, len(targets)), Changed: false}
	for _, target := range targets {
		fileResult, err := uninstallHookFile(ctx, target.client, target.path, signatures, knownCommands)
		if err != nil {
			slog.WarnContext(ctx, "hooks.uninstall_failed", "component", "hookspec", "client", target.client, "err", err)
			return UninstallResult{}, err
		}
		result.Changed = result.Changed || fileResult.Changed
		result.Files = append(result.Files, fileResult)
	}
	slog.InfoContext(ctx, "hooks.uninstall_completed", "component", "hookspec", "changed", result.Changed)
	return result, nil
}

func uninstallHookFile(ctx context.Context, client Client, path string, signatures [][]string, knownCommands []string) (UninstallFileResult, error) {
	slog.DebugContext(ctx, "hooks.uninstall_file_started", "component", "hookspec", "client", client, "path", path)
	result := UninstallFileResult{Client: client, SettingsPath: path, Changed: false}
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "hooks.uninstall_file_failed", "component", "hookspec", "client", client, "path", path, "err", err)
		return UninstallFileResult{}, fmt.Errorf("uninstall %s hooks canceled: %w", client, err)
	}
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		slog.WarnContext(ctx, "hooks.uninstall_file_failed", "component", "hookspec", "client", client, "path", path, "err", err)
		return UninstallFileResult{}, fmt.Errorf("read %s hooks %s: %w", client, path, err)
	}
	var body []byte
	switch client {
	case ClientClaudeCode:
		body, err = uninstallClaudeHooks(existing, signatures, knownCommands)
	case ClientCodex:
		body = []byte(removeCodexCommandHookGroupsExact(removeCodexManagedBlock(string(existing)), signatures, knownCommands))
	case ClientCursor:
		body, err = uninstallCursorHooks(existing, signatures, knownCommands)
	case ClientAll:
		return UninstallFileResult{}, fmt.Errorf("uninstall client must be concrete, got %q", client)
	default:
		return UninstallFileResult{}, fmt.Errorf("unsupported hooks client %q", client)
	}
	if err != nil {
		slog.WarnContext(ctx, "hooks.uninstall_file_failed", "component", "hookspec", "client", client, "path", path, "err", err)
		return UninstallFileResult{}, err
	}
	if bytes.Equal(existing, body) {
		return result, nil
	}
	// #nosec G703 -- path is one fixed settings path below the resolved home directory.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		slog.WarnContext(ctx, "hooks.uninstall_file_failed", "component", "hookspec", "client", client, "path", path, "err", err)
		return UninstallFileResult{}, fmt.Errorf("write %s hooks %s: %w", client, path, err)
	}
	result.Changed = true
	return result, nil
}

func uninstallClaudeHooks(existing []byte, signatures [][]string, knownCommands []string) ([]byte, error) {
	document, err := unmarshalClaudeSettingsDocument(existing)
	if err != nil {
		return nil, err
	}
	hooksByEvent, err := document.unmarshalClaudeCodeHooks()
	if err != nil {
		return nil, err
	}
	before, err := json.Marshal(hooksByEvent)
	if err != nil {
		slog.Warn("hooks.uninstall_claude_marshal_failed", "err", err)
		return nil, fmt.Errorf("marshal existing Claude Code hooks: %w", err)
	}
	for eventName, groups := range hooksByEvent {
		hooksByEvent[eventName] = removeClaudeHookHandlersWithMatcher(groups, signatures, knownCommands)
	}
	after, err := json.Marshal(hooksByEvent)
	if err != nil {
		slog.Warn("hooks.uninstall_claude_marshal_failed", "err", err)
		return nil, fmt.Errorf("marshal filtered Claude Code hooks: %w", err)
	}
	if bytes.Equal(before, after) {
		return existing, nil
	}
	document.fields["hooks"] = after
	return document.MarshalJSON()
}

func uninstallCursorHooks(existing []byte, signatures [][]string, knownCommands []string) ([]byte, error) {
	document, err := unmarshalCursorHooksDocument(existing)
	if err != nil {
		return nil, err
	}
	hooksByEvent, err := document.unmarshalCursorHooks()
	if err != nil {
		return nil, err
	}
	before, err := json.Marshal(hooksByEvent)
	if err != nil {
		slog.Warn("hooks.uninstall_cursor_marshal_failed", "err", err)
		return nil, fmt.Errorf("marshal existing Cursor hooks: %w", err)
	}
	for eventName, handlers := range hooksByEvent {
		hooksByEvent[eventName] = removeCursorHookHandlersWithMatcher(handlers, signatures, knownCommands)
	}
	after, err := json.Marshal(hooksByEvent)
	if err != nil {
		slog.Warn("hooks.uninstall_cursor_marshal_failed", "err", err)
		return nil, fmt.Errorf("marshal filtered Cursor hooks: %w", err)
	}
	if bytes.Equal(before, after) {
		return existing, nil
	}
	document.fields["hooks"] = after
	return document.MarshalJSON()
}
