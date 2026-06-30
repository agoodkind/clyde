package cursorjsonl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/homedir"
)

const (
	cursorProjectsDirsEnvVar = "CLYDE_CURSOR_PROJECTS_DIRS"
	cursorProjectsSubdir     = ".cursor/projects"
	agentTranscriptsDirName  = "agent-transcripts"
	jsonlExtension           = ".jsonl"
)

// ProjectRoot names one Cursor modern transcript project root.
type ProjectRoot struct {
	Path string
}

// TranscriptFile names one Cursor modern transcript file discovered on disk.
type TranscriptFile struct {
	Path           string
	ConversationID string
	ProjectKey     string
}

// ResolveProjectRoots resolves the Cursor modern transcript roots from
// CLYDE_CURSOR_PROJECTS_DIRS or the default ~/.cursor/projects directory.
func ResolveProjectRoots() ([]ProjectRoot, error) {
	configuredRoots := strings.TrimSpace(os.Getenv(cursorProjectsDirsEnvVar))
	if configuredRoots == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve cursor projects home: %w", err)
		}
		configuredRoots = filepath.Join(userHome, cursorProjectsSubdir)
	}

	seen := make(map[string]bool)
	roots := make([]ProjectRoot, 0, 1)
	for _, rawEntry := range filepath.SplitList(configuredRoots) {
		rootPath := strings.TrimSpace(rawEntry)
		if rootPath == "" {
			continue
		}
		if strings.HasPrefix(rootPath, "~") {
			rootPath = homedir.Expand(rootPath)
		}
		absolutePath, err := filepath.Abs(rootPath)
		if err == nil {
			rootPath = absolutePath
		}
		rootPath = filepath.Clean(rootPath)
		if seen[rootPath] {
			continue
		}
		seen[rootPath] = true
		roots = append(roots, ProjectRoot{Path: rootPath})
	}
	return roots, nil
}

// DiscoverTranscriptFiles walks Cursor modern transcript roots and returns
// files matching <projectKey>/agent-transcripts/<uuid>/<uuid>.jsonl.
func DiscoverTranscriptFiles(roots []ProjectRoot) ([]TranscriptFile, error) {
	files := make([]TranscriptFile, 0)
	for _, root := range roots {
		err := filepath.WalkDir(root.Path, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrNotExist) || errors.Is(walkErr, os.ErrPermission) {
					return nil
				}
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			file, ok := transcriptFileFromPath(root.Path, path)
			if !ok {
				return nil
			}
			files = append(files, file)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk cursor transcript root %s: %w", root.Path, err)
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// WorkspacePathFromProjectKey returns a lossy filesystem-path display hint from
// a Cursor project key. Real directory names can contain dashes, so callers must
// treat the raw ProjectKey and conversation UUID as the reliable identifiers.
func WorkspacePathFromProjectKey(projectKey string) string {
	trimmed := strings.Trim(strings.TrimSpace(projectKey), "-")
	if trimmed == "" {
		return ""
	}
	return "/" + strings.ReplaceAll(trimmed, "-", "/")
}

func transcriptFileFromPath(rootPath string, path string) (TranscriptFile, bool) {
	if filepath.Ext(path) != jsonlExtension {
		return TranscriptFile{}, false
	}
	relativePath, err := filepath.Rel(rootPath, path)
	if err != nil {
		return TranscriptFile{}, false
	}
	parts := splitPathParts(relativePath)
	if len(parts) != 4 {
		return TranscriptFile{}, false
	}
	projectKey := parts[0]
	if parts[1] != agentTranscriptsDirName {
		return TranscriptFile{}, false
	}
	conversationID := parts[2]
	filenameStem := strings.TrimSuffix(parts[3], jsonlExtension)
	if conversationID == "" || filenameStem != conversationID {
		return TranscriptFile{}, false
	}
	return TranscriptFile{
		Path:           path,
		ConversationID: conversationID,
		ProjectKey:     projectKey,
	}, true
}

func splitPathParts(path string) []string {
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return nil
	}
	return strings.Split(filepath.ToSlash(cleaned), "/")
}
