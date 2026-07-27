package cursorstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// WorkspaceJSONPath is empty for a workspace Cursor stored without one, which is
// what a window opened on no folder looks like.
type WorkspaceEntry struct {
	WorkspaceHash     string
	StateDBPath       string
	WorkspaceJSONPath string
}

// WorkspaceListing is what one data root's workspaceStorage directory holds: the
// workspace databases Clyde can name, and how much of the directory it could not
// read.
//
// The counts exist because a search across every workspace has to know whether
// its miss covered the whole directory. A directory entry Clyde could not examine
// is a workspace whose database was never opened, so a miss over it proves
// nothing about what that database holds.
type WorkspaceListing struct {
	Entries []WorkspaceEntry
	// Unreadable counts directory entries this listing could not examine. A
	// directory that simply holds no state.vscdb is not counted, because it holds
	// nothing a search could have missed.
	Unreadable int
	// StorageDirMissing reports that the workspaceStorage directory does not
	// exist. That is an answer rather than a failure: this machine has no workspace
	// databases, so a miss across them is a real absence rather than an unread one.
	StorageDirMissing bool
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
//
// Every directory holding a `state.vscdb` is listed, including the ones with no
// `workspace.json` beside it. The descriptor names the folder a workspace is open
// on and nothing else, so requiring it drops a workspace that has no folder
// rather than one that has no data: Cursor stores a window opened on no folder
// under `workspaceStorage/empty-window`, with no descriptor and its own
// `aiService.generations` ring. On this machine that directory is one of 123 and
// holds a full ring of 50 request ids, every one of which a listing that required
// the descriptor put out of reach.
//
// A directory entry that cannot be examined is counted rather than skipped
// quietly, so a search across the workspaces can tell a miss over everything from
// a miss over what it managed to read.
func (root DataRoot) ListWorkspaceEntries() (WorkspaceListing, error) {
	listing := WorkspaceListing{Entries: nil, Unreadable: 0, StorageDirMissing: false}

	entries, err := os.ReadDir(root.WorkspaceStorageDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			listing.StorageDirMissing = true
			return listing, nil
		}
		slog.Warn("providers.cursor.store.workspace_storage_read_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return listing, fmt.Errorf("read cursor workspace storage dir %s: %w", root.WorkspaceStorageDir, err)
	}

	out := make([]WorkspaceEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		workspaceHash := entry.Name()
		workspaceDir := filepath.Join(root.WorkspaceStorageDir, workspaceHash)
		stateDBPath := filepath.Join(workspaceDir, cursorWorkspaceDBName)
		present, statErr := filePresent(stateDBPath)
		if statErr != nil {
			listing.Unreadable++
			continue
		}
		if !present {
			continue
		}
		out = append(out, WorkspaceEntry{
			WorkspaceHash:     workspaceHash,
			StateDBPath:       stateDBPath,
			WorkspaceJSONPath: workspaceDescriptorPath(workspaceDir),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].WorkspaceHash < out[j].WorkspaceHash
	})
	listing.Entries = out
	return listing, nil
}

// workspaceDescriptorPath returns the folder descriptor beside one workspace
// database, or the empty string when the workspace has none.
//
// A stat that fails for any reason other than absence yields the path anyway. An
// empty path here means "this workspace has no folder", which is a claim about
// the workspace, while a descriptor Clyde was denied is a claim about Clyde, and
// collapsing the two indexes the conversation with an empty workspace root and no
// sign that anything went wrong. Returning the path sends the real read at it,
// which reports the real failure.
func workspaceDescriptorPath(workspaceDir string) string {
	descriptorPath := filepath.Join(workspaceDir, cursorWorkspaceJSONName)
	present, err := filePresent(descriptorPath)
	if err != nil {
		return descriptorPath
	}
	if !present {
		return ""
	}
	return descriptorPath
}

// ReadWorkspaceFolderPath reads one Cursor workspace.json file and returns its
// filesystem folder path.
func ReadWorkspaceFolderPath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("providers.cursor.store.workspace_descriptor_read_failed", "concern", concern, "path", path, "err", err)
		return "", fmt.Errorf("read cursor workspace descriptor %s: %w", path, err)
	}
	descriptor, err := decodeWorkspaceDescriptorJSON(data)
	if err != nil {
		slog.Warn("providers.cursor.store.workspace_descriptor_decode_failed", "concern", concern, "path", path, "err", err)
		return "", fmt.Errorf("decode cursor workspace descriptor %s: %w", path, err)
	}
	folderPath, err := fileURIToPath(descriptor.Folder)
	if err != nil {
		slog.Warn("providers.cursor.store.workspace_folder_decode_failed", "concern", concern, "path", path, "err", err)
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
	if runtime.GOOS == "darwin" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.home_resolve_failed", "concern", concern, "goos", runtime.GOOS, "err", err)
			return "", fmt.Errorf("resolve cursor home: %w", err)
		}
		return filepath.Join(userHome, "Library", "Application Support", "Cursor", "User"), nil
	}
	if runtime.GOOS == "linux" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.home_resolve_failed", "concern", concern, "goos", runtime.GOOS, "err", err)
			return "", fmt.Errorf("resolve cursor home: %w", err)
		}
		return filepath.Join(userHome, ".config", "Cursor", "User"), nil
	}
	if runtime.GOOS == "windows" {
		appData := strings.TrimSpace(os.Getenv("APPDATA"))
		if appData != "" {
			return filepath.Join(appData, "Cursor", "User"), nil
		}
		userHome, err := os.UserHomeDir()
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.home_resolve_failed", "concern", concern, "goos", runtime.GOOS, "err", err)
			return "", fmt.Errorf("resolve cursor home: %w", err)
		}
		return filepath.Join(userHome, "AppData", "Roaming", "Cursor", "User"), nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.home_resolve_failed", "concern", concern, "goos", runtime.GOOS, "err", err)
		return "", fmt.Errorf("resolve cursor home for %s: %w", runtime.GOOS, err)
	}
	return filepath.Join(userHome, ".config", "Cursor", "User"), nil
}

// filePresent reports whether a path exists, and separates that from failing to
// find out. Collapsing the two turns a directory Clyde was denied into one that
// holds nothing, which reads downstream as a confirmed absence of whatever the
// caller was looking for.
func filePresent(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	slog.Warn("providers.cursor.store.file_stat_failed", "concern", concern, "path", path, "err", err)
	return false, fmt.Errorf("stat cursor path %s: %w", path, err)
}

func fileURIToPath(folder string) (string, error) {
	trimmed := strings.TrimSpace(folder)
	if trimmed == "" {
		return "", nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		slog.Warn("providers.cursor.store.file_uri_parse_failed", "concern", concern, "folder", folder, "err", err)
		return "", fmt.Errorf("parse file uri %q: %w", folder, err)
	}
	if parsed.Scheme != "file" {
		return trimmed, nil
	}
	folderPath := parsed.Path
	if parsed.Host != "" && parsed.Host != "localhost" {
		folderPath = "//" + parsed.Host + parsed.Path
	}
	if runtime.GOOS == "windows" && isWindowsDrivePath(folderPath) {
		folderPath = strings.TrimPrefix(folderPath, "/")
	}
	return filepath.FromSlash(folderPath), nil
}

func isWindowsDrivePath(path string) bool {
	if len(path) < 3 || path[0] != '/' || path[2] != ':' {
		return false
	}
	driveLetter := path[1]
	return driveLetter >= 'A' && driveLetter <= 'Z' || driveLetter >= 'a' && driveLetter <= 'z'
}
