// Package cursorjsonl reads Cursor's modern JSONL transcript artifacts.
package cursorjsonl

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/homedir"
)

const (
	concern = "providers.cursor.jsonl"

	cursorProjectsDirsEnvVar = "CLYDE_CURSOR_PROJECTS_DIRS"
	cursorProjectsSubdir     = ".cursor/projects"
	agentTranscriptsDirName  = "agent-transcripts"
	subagentsDirName         = "subagents"
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
	// ParentConversationID names the conversation that owns this transcript, set
	// only for a subagent transcript, where the containing directory is literally
	// the parent conversation's id. It is empty for a conversation's own
	// transcript.
	ParentConversationID string
	// TwinSubagentParentID names the conversation that dispatched this uuid when
	// this is the top-level twin of a subagent transcript. Cursor writes a
	// dispatched conversation's transcript twice: under the dispatching
	// conversation's subagents/ directory and again as a top-level
	// <uuid>/<uuid>.jsonl in the same project, and the twin carries no marker of
	// its own, so the subagents/ copy is what says the uuid was dispatched. It is
	// empty for a subagent transcript itself and for a top-level transcript with
	// no subagents/ twin in the project.
	TwinSubagentParentID string
}

// ResolveProjectRoots resolves the Cursor modern transcript roots from
// CLYDE_CURSOR_PROJECTS_DIRS or the default ~/.cursor/projects directory.
func ResolveProjectRoots() ([]ProjectRoot, error) {
	configuredRoots := strings.TrimSpace(os.Getenv(cursorProjectsDirsEnvVar))
	if configuredRoots == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			slog.Warn("providers.cursor.jsonl.projects_home_resolve_failed", "concern", concern, "err", err)
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

// MatchTranscriptFile resolves one absolute JSONL path against configured
// project roots without walking the filesystem.
func MatchTranscriptFile(path string) (TranscriptFile, bool, error) {
	roots, err := ResolveProjectRoots()
	if err != nil {
		return TranscriptFile{}, false, err
	}
	for _, root := range roots {
		file, ok := transcriptFileFromPath(root.Path, path)
		if ok {
			if file.ParentConversationID == "" {
				file.TwinSubagentParentID = findSubagentTwinParent(file.Path, file.ConversationID)
			}
			return file, true, nil
		}
	}
	var emptyFile TranscriptFile
	return emptyFile, false, nil
}

// DiscoverTranscriptFiles walks Cursor modern transcript roots and returns files
// matching either <projectKey>/agent-transcripts/<uuid>/<uuid>.jsonl, a
// conversation's own transcript, or
// <projectKey>/agent-transcripts/<parent-uuid>/subagents/<uuid>.jsonl, a subagent
// transcript whose containing directory names the conversation that owns it.
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
			slog.Warn("providers.cursor.jsonl.transcript_root_walk_failed", "concern", concern, "path", root.Path, "err", err)
			return nil, fmt.Errorf("walk cursor transcript root %s: %w", root.Path, err)
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	fillTwinSubagentParents(files)
	return files, nil
}

// projectConversationKey scopes a conversation uuid to the project it was
// discovered in, because the subagents/ copy and its top-level twin always sit
// in the same project directory and a uuid coincidence across projects must not
// reclassify an unrelated conversation.
type projectConversationKey struct {
	ProjectKey     string
	ConversationID string
}

// fillTwinSubagentParents marks each top-level transcript whose uuid also
// appears under a subagents/ directory in the same project, carrying the
// dispatching conversation's id onto the twin.
func fillTwinSubagentParents(files []TranscriptFile) {
	subagentParentByKey := make(map[projectConversationKey]string)
	for _, file := range files {
		if file.ParentConversationID == "" {
			continue
		}
		key := projectConversationKey{ProjectKey: file.ProjectKey, ConversationID: file.ConversationID}
		if _, exists := subagentParentByKey[key]; !exists {
			subagentParentByKey[key] = file.ParentConversationID
		}
	}
	if len(subagentParentByKey) == 0 {
		return
	}
	for i := range files {
		if files[i].ParentConversationID != "" {
			continue
		}
		key := projectConversationKey{ProjectKey: files[i].ProjectKey, ConversationID: files[i].ConversationID}
		files[i].TwinSubagentParentID = subagentParentByKey[key]
	}
}

// findSubagentTwinParent answers the same question as fillTwinSubagentParents
// for one path resolved outside a discovery walk: it looks for this uuid under
// a sibling conversation's subagents/ directory in the same project and returns
// that conversation's id, or an empty string when no twin exists.
func findSubagentTwinParent(topLevelPath string, conversationID string) string {
	agentTranscriptsDir := filepath.Dir(filepath.Dir(topLevelPath))
	pattern := filepath.Join(agentTranscriptsDir, "*", subagentsDirName, conversationID+jsonlExtension)
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	return filepath.Base(filepath.Dir(filepath.Dir(matches[0])))
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
	var emptyFile TranscriptFile

	if filepath.Ext(path) != jsonlExtension {
		return emptyFile, false
	}
	relativePath, err := filepath.Rel(rootPath, path)
	if err != nil {
		return emptyFile, false
	}
	if relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return emptyFile, false
	}
	parts := splitPathParts(relativePath)
	if len(parts) < 4 || parts[1] != agentTranscriptsDirName {
		return emptyFile, false
	}
	projectKey := parts[0]
	conversationDirName := parts[2]
	if conversationDirName == "" {
		return emptyFile, false
	}

	// A conversation's own transcript: <projectKey>/agent-transcripts/<id>/<id>.jsonl.
	if len(parts) == 4 {
		filenameStem := strings.TrimSuffix(parts[3], jsonlExtension)
		if filenameStem != conversationDirName {
			return emptyFile, false
		}
		return TranscriptFile{
			Path:                 path,
			ConversationID:       conversationDirName,
			ProjectKey:           projectKey,
			ParentConversationID: "",
			// The twin check needs the sibling files, so DiscoverTranscriptFiles and
			// MatchTranscriptFile fill this after the path is matched.
			TwinSubagentParentID: "",
		}, true
	}

	// A subagent transcript, one level deeper under a literal subagents/
	// directory: <projectKey>/agent-transcripts/<parent-id>/subagents/<own-id>.jsonl.
	// The shape is pinned exactly so no other file that happens to sit under
	// agent-transcripts/ becomes a conversation. The file is named with its own
	// uuid, so its derived id does not collide with the parent's.
	if len(parts) != 5 || parts[3] != subagentsDirName {
		return emptyFile, false
	}
	subagentID := strings.TrimSuffix(parts[4], jsonlExtension)
	if subagentID == "" {
		return emptyFile, false
	}
	return TranscriptFile{
		Path:                 path,
		ConversationID:       subagentID,
		ProjectKey:           projectKey,
		ParentConversationID: conversationDirName,
		// A subagent transcript is its own marker; the twin field is only ever
		// set on a top-level transcript.
		TwinSubagentParentID: "",
	}, true
}

func splitPathParts(path string) []string {
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return nil
	}
	return strings.Split(filepath.ToSlash(cleaned), "/")
}
