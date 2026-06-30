package cursorstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/homedir"
)

const (
	cursorDataDirsEnvVar       = "CLYDE_CURSOR_DATA_DIRS"
	cursorGlobalDBRelative     = "globalStorage/state.vscdb"
	cursorWorkspaceStorageName = "workspaceStorage"
	cursorWorkspaceDBName      = "state.vscdb"
	cursorWorkspaceJSONName    = "workspace.json"
)

// DataRoot names one local Cursor user data root and its known database paths.
type DataRoot struct {
	RootDir             string
	GlobalDBPath        string
	WorkspaceStorageDir string
}

// WorkspaceEntry names one Cursor workspace database and its folder descriptor.
type WorkspaceEntry struct {
	WorkspaceHash     string
	StateDBPath       string
	WorkspaceJSONPath string
}

type workspaceDescriptor struct {
	Folder string `json:"folder"`
}

// ResolveDataRoots resolves the configured or default Cursor data roots Clyde
// should inspect locally.
func ResolveDataRoots(ctx context.Context, dataDirs string) ([]DataRoot, error) {
	roots, err := resolveDataDirList(ctx, dataDirs)
	if err != nil {
		return nil, err
	}
	out := make([]DataRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, DataRoot{
			RootDir:             root,
			GlobalDBPath:        filepath.Join(root, cursorGlobalDBRelative),
			WorkspaceStorageDir: filepath.Join(root, cursorWorkspaceStorageName),
		})
	}
	return out, nil
}

// ResolveDataRootsFromEnv resolves the Cursor data roots from
// `CLYDE_CURSOR_DATA_DIRS`.
func ResolveDataRootsFromEnv(ctx context.Context) ([]DataRoot, error) {
	return ResolveDataRoots(ctx, os.Getenv(cursorDataDirsEnvVar))
}

// ListWorkspaceEntries lists Cursor workspace databases below one data root.
func (root DataRoot) ListWorkspaceEntries() ([]WorkspaceEntry, error) {
	entries, err := os.ReadDir(root.WorkspaceStorageDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cursor workspace storage dir %s: %w", root.WorkspaceStorageDir, err)
	}

	out := make([]WorkspaceEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		workspaceHash := entry.Name()
		workspaceDir := filepath.Join(root.WorkspaceStorageDir, workspaceHash)
		stateDBPath := filepath.Join(workspaceDir, cursorWorkspaceDBName)
		if !fileExists(stateDBPath) {
			continue
		}
		workspaceJSONPath := filepath.Join(workspaceDir, cursorWorkspaceJSONName)
		if !fileExists(workspaceJSONPath) {
			continue
		}
		out = append(out, WorkspaceEntry{
			WorkspaceHash:     workspaceHash,
			StateDBPath:       stateDBPath,
			WorkspaceJSONPath: workspaceJSONPath,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].WorkspaceHash < out[j].WorkspaceHash
	})
	return out, nil
}

// ReadWorkspaceFolderPath reads one Cursor workspace.json file and returns its
// filesystem folder path.
func ReadWorkspaceFolderPath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read cursor workspace descriptor %s: %w", path, err)
	}
	descriptor, err := decodeWorkspaceDescriptorJSON(data)
	if err != nil {
		return "", fmt.Errorf("decode cursor workspace descriptor %s: %w", path, err)
	}
	folderPath, err := fileURIToPath(descriptor.Folder)
	if err != nil {
		return "", fmt.Errorf("decode cursor workspace folder %s: %w", path, err)
	}
	return folderPath, nil
}

func resolveDataDirList(ctx context.Context, dataDirs string) ([]string, error) {
	raw := strings.TrimSpace(dataDirs)
	if raw == "" {
		defaultDir, err := defaultCursorDataDir(ctx)
		if err != nil {
			return nil, err
		}
		raw = defaultDir
	}

	seen := make(map[string]bool)
	out := make([]string, 0, 1)
	for _, entry := range filepath.SplitList(raw) {
		root := strings.TrimSpace(entry)
		if root == "" {
			continue
		}
		if strings.HasPrefix(root, "~") {
			root = homedir.Expand(root)
		}
		absoluteRoot, err := filepath.Abs(root)
		if err == nil {
			root = absoluteRoot
		}
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	return out, nil
}

func defaultCursorDataDir(ctx context.Context) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve cursor home: %w", err)
		}
		return filepath.Join(userHome, "Library", "Application Support", "Cursor", "User"), nil
	case "linux":
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve cursor home: %w", err)
		}
		return filepath.Join(userHome, ".config", "Cursor", "User"), nil
	case "windows":
		appData := strings.TrimSpace(os.Getenv("APPDATA"))
		if appData != "" {
			return filepath.Join(appData, "Cursor", "User"), nil
		}
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve cursor home: %w", err)
		}
		return filepath.Join(userHome, "AppData", "Roaming", "Cursor", "User"), nil
	default:
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve cursor home for %s: %w", runtime.GOOS, err)
		}
		_ = ctx
		return filepath.Join(userHome, ".config", "Cursor", "User"), nil
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileURIToPath(folder string) (string, error) {
	trimmed := strings.TrimSpace(folder)
	if trimmed == "" {
		return "", nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse file uri %q: %w", folder, err)
	}
	if parsed.Scheme != "file" {
		return trimmed, nil
	}
	folderPath := parsed.Path
	if parsed.Host != "" && parsed.Host != "localhost" {
		folderPath = "//" + parsed.Host + parsed.Path
	}
	if runtime.GOOS == "windows" {
		folderPath = strings.TrimPrefix(folderPath, "/")
	}
	return filepath.FromSlash(folderPath), nil
}
