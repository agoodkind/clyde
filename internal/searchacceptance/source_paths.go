package searchacceptance

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	zedparser "goodkind.io/clyde/internal/providers/zed/parser"
)

const (
	frozenCursorRoot = "Library/Application Support/Cursor/User"
	frozenZedRoot    = "Library/Application Support/Zed"
)

// FrozenSourceEnvironment configures a provider's frozen physical data root.
type FrozenSourceEnvironment struct {
	Name  string
	Value string
}

// MapFrozenRecord changes only the read path. Manifest keys retain their exact
// frozen spelling, including the ./ prefix. Snapshot verification checks files
// and symlink containment before this pure mapper is used.
func MapFrozenRecord(snapshotRoot, originalHome string, record conversation.Record, manifestFiles map[string]bool) (mapped conversation.Record, err error) {
	mapped = record
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.source_path_rejected", "component", "searchacceptance", "concern", "source", "conversation_id", record.ID, "err", err)
		}
	}()
	if !cleanAbsoluteRoot(snapshotRoot) || !cleanAbsoluteRoot(originalHome) {
		return mapped, errors.New("frozen root and original home must be clean absolute directories")
	}
	switch {
	case strings.HasPrefix(record.ArtifactPath, "cursor://"):
		mapped.ArtifactPath, err = mapFrozenCursor(snapshotRoot, originalHome, record, manifestFiles)
	case strings.HasPrefix(record.ArtifactPath, "zed://"):
		mapped.ArtifactPath, err = mapFrozenZed(snapshotRoot, originalHome, record, manifestFiles)
	default:
		mapped.ArtifactPath, err = mapFrozenPhysical(snapshotRoot, originalHome, record, manifestFiles)
	}
	return mapped, err
}

// FrozenProviderEnvironment returns an explicit provider data root without
// changing the process environment. Physical transcript providers need no value.
func FrozenProviderEnvironment(snapshotRoot string, provider conversation.Provider) (FrozenSourceEnvironment, error) {
	if !cleanAbsoluteRoot(snapshotRoot) {
		return FrozenSourceEnvironment{}, errors.New("frozen root must be a clean absolute directory")
	}
	switch provider {
	case conversation.ProviderCursor:
		return FrozenSourceEnvironment{Name: "CLYDE_CURSOR_DATA_DIRS", Value: filepath.Join(snapshotRoot, "home", frozenCursorRoot)}, nil
	case conversation.ProviderZed:
		return FrozenSourceEnvironment{Name: "CLYDE_ZED_DATA_DIRS", Value: filepath.Join(snapshotRoot, "home", frozenZedRoot)}, nil
	case conversation.ProviderClaude, conversation.ProviderCodex, conversation.ProviderCopilot:
		return FrozenSourceEnvironment{Name: "", Value: ""}, nil
	default:
		return FrozenSourceEnvironment{}, fmt.Errorf("unsupported frozen provider %q", provider.String())
	}
}

// FrozenSourceEnvironments binds both Cursor source formats and Zed to the snapshot.
func FrozenSourceEnvironments(snapshotRoot string) ([]FrozenSourceEnvironment, error) {
	if !cleanAbsoluteRoot(snapshotRoot) {
		return nil, errors.New("frozen root must be a clean absolute directory")
	}
	environments := []FrozenSourceEnvironment{
		{Name: "CLYDE_CURSOR_PROJECTS_DIRS", Value: filepath.Join(snapshotRoot, "home/.cursor/projects")},
	}
	for _, provider := range []conversation.Provider{conversation.ProviderCursor, conversation.ProviderZed} {
		environment, err := FrozenProviderEnvironment(snapshotRoot, provider)
		if err != nil {
			return nil, err
		}
		environments = append(environments, environment)
	}
	return environments, nil
}

func cleanAbsoluteRoot(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
}

func mapFrozenPhysical(snapshotRoot, originalHome string, record conversation.Record, manifest map[string]bool) (string, error) {
	path := record.ArtifactPath
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, originalHome+string(filepath.Separator)) {
		return "", errors.New("physical artifact must use its original home path without traversal")
	}
	relative := strings.TrimPrefix(path, originalHome+string(filepath.Separator))
	var prefix string
	switch record.Provider {
	case conversation.ProviderClaude:
		prefix = ".claude/"
	case conversation.ProviderCodex:
		prefix = ".codex/"
	case conversation.ProviderCopilot:
		prefix = ".copilot/"
	case conversation.ProviderCursor:
		prefix = ".cursor/"
	default:
		return "", fmt.Errorf("unsupported physical provider %q", record.Provider.String())
	}
	if !strings.HasPrefix(filepath.ToSlash(relative), prefix) {
		return "", errors.New("physical artifact path does not match its provider")
	}
	return frozenManifestPath(snapshotRoot, filepath.ToSlash(filepath.Join("home", relative)), manifest)
}

func frozenManifestPath(snapshotRoot, relative string, manifest map[string]bool) (string, error) {
	if filepath.IsAbs(relative) || filepath.ToSlash(filepath.Clean(relative)) != relative || strings.HasPrefix(relative, "../") || relative == ".." {
		return "", errors.New("frozen source path escapes its root")
	}
	if !manifest["./"+relative] {
		return "", fmt.Errorf("frozen source is absent from the manifest: %q", relative)
	}
	return filepath.Join(snapshotRoot, filepath.FromSlash(relative)), nil
}

func mapFrozenCursor(snapshotRoot, originalHome string, record conversation.Record, manifest map[string]bool) (string, error) {
	if record.Provider != conversation.ProviderCursor {
		return "", errors.New("virtual Cursor path requires the Cursor provider")
	}
	parsed, err := cursorparser.ParseVirtualPath(record.ArtifactPath)
	if err != nil {
		slog.Warn("search.acceptance.cursor_path_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		return "", fmt.Errorf("parse frozen Cursor path: %w", err)
	}
	if parsed.RootHash != cursorparser.RootHash(filepath.Join(originalHome, frozenCursorRoot)) || parsed.Kind != cursorparser.VirtualKindComposer {
		return "", errors.New("virtual Cursor path has an unsupported root or kind")
	}
	if _, err := frozenManifestPath(snapshotRoot, "home/"+frozenCursorRoot+"/globalStorage/state.vscdb", manifest); err != nil {
		return "", err
	}
	root := filepath.Join(snapshotRoot, "home", frozenCursorRoot)
	return cursorparser.BuildVirtualPath(cursorparser.RootHash(root), parsed.Kind, parsed.ID), nil
}

func mapFrozenZed(snapshotRoot, originalHome string, record conversation.Record, manifest map[string]bool) (string, error) {
	if record.Provider != conversation.ProviderZed {
		return "", errors.New("virtual Zed path requires the Zed provider")
	}
	parsed, err := zedparser.ParseVirtualPath(record.ArtifactPath)
	if err != nil {
		slog.Warn("search.acceptance.zed_path_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		return "", fmt.Errorf("parse frozen Zed path: %w", err)
	}
	if parsed.RootHash != zedparser.RootHash(filepath.Join(originalHome, frozenZedRoot)) || parsed.Channel != "0-stable" || parsed.SessionID == "." || parsed.SessionID == ".." {
		return "", errors.New("virtual Zed path has an unsupported root or channel")
	}
	for _, relative := range []string{"home/" + frozenZedRoot + "/threads/threads.db", "home/" + frozenZedRoot + "/db/" + parsed.Channel + "/db.sqlite"} {
		if _, err := frozenManifestPath(snapshotRoot, relative, manifest); err != nil {
			return "", err
		}
	}
	root := filepath.Join(snapshotRoot, "home", frozenZedRoot)
	return "zed://" + zedparser.RootHash(root) + "/" + parsed.Channel + "/" + parsed.SessionID, nil
}
