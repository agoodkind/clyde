// Package memorydocs reads the durable per-agent memory notes that Claude and
// Codex keep outside their conversation transcripts. Reorient uses these as a
// fallback and enrichment source when artifact lineage alone does not recover
// enough prior context. This package only reads; it never writes provider files.
package memorydocs

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/providerid"
)

// indexFilename is the name both providers use for the memory index that holds
// one-line hooks per topic file.
const indexFilename = "MEMORY.md"

// Doc is one memory note recovered for reorient. Body holds the full file text;
// reorient bounds and pages it, so this package does not truncate.
type Doc struct {
	Provider string `json:"provider"`
	Title    string `json:"title"`
	Path     string `json:"path"`
	Body     string `json:"body"`
	// Index is true for the provider's MEMORY.md index file, which lists every
	// topic with a one-line hook. Reorient shows the index by default and the
	// per-topic bodies when a topic narrows them.
	Index bool `json:"index"`
}

// Load returns the memory docs for a conversation's provider. Claude memory is
// project-scoped under the workspace root; Codex memory is global, so
// workspaceRoot is ignored for Codex. A missing memory directory is not an
// error: it yields no docs, because many workspaces have no saved memory. Files
// that cannot be read are skipped rather than failing the whole load, so a
// single bad note never blocks reorient.
func Load(provider providerid.Provider, workspaceRoot string) ([]Doc, error) {
	dir, ok := memoryDir(provider, workspaceRoot)
	if !ok {
		return nil, nil
	}
	return loadDir(dir, provider.String())
}

// memoryDir resolves the on-disk memory directory for a provider. The second
// result is false when the provider has no memory location or the home dir is
// unavailable.
func memoryDir(provider providerid.Provider, workspaceRoot string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	switch provider {
	case providerid.ProviderClaude:
		workspaceRoot = strings.TrimSpace(workspaceRoot)
		if workspaceRoot == "" {
			return "", false
		}
		return filepath.Join(home, ".claude", "projects", mangleProjectPath(workspaceRoot), "memory"), true
	case providerid.ProviderCodex:
		return filepath.Join(home, ".codex", "memories"), true
	case providerid.ProviderUnspecified,
		providerid.ProviderAnthropic,
		providerid.ProviderOpenAICompat,
		providerid.ProviderMITM,
		providerid.ProviderArtifact,
		providerid.ProviderCursor:
		return "", false
	default:
		return "", false
	}
}

// mangleProjectPath encodes a workspace root the way Claude names its per-project
// directory under ~/.claude/projects, replacing each path separator and dot with
// a dash (for example /Users/me/Sites/app becomes -Users-me-Sites-app).
func mangleProjectPath(workspaceRoot string) string {
	cleaned := filepath.Clean(workspaceRoot)
	replacer := strings.NewReplacer(string(os.PathSeparator), "-", ".", "-")
	return replacer.Replace(cleaned)
}

func loadDir(dir string, provider string) ([]Doc, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		slog.Warn("conversation.memorydocs.read_dir_failed", "concern", "conversation.reorient", "component", "conversation", "dir", dir, "err", err)
		return nil, fmt.Errorf("read memory dir %s: %w", dir, err)
	}
	docs := make([]Doc, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		docs = append(docs, Doc{
			Provider: provider,
			Title:    strings.TrimSuffix(name, filepath.Ext(name)),
			Path:     path,
			Body:     string(body),
			Index:    name == indexFilename,
		})
	}
	sortDocs(docs)
	return docs, nil
}

// sortDocs puts the index first, then orders the rest by title so reorient
// emits memory evidence deterministically across paged calls.
func sortDocs(docs []Doc) {
	sort.SliceStable(docs, func(i int, j int) bool {
		if docs[i].Index != docs[j].Index {
			return docs[i].Index
		}
		return docs[i].Title < docs[j].Title
	})
}
