package memorydocs

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/providerid"
)

func TestMangleProjectPath(t *testing.T) {
	t.Parallel()
	got := mangleProjectPath("/Users/me/Sites/clyde-dev/clyde")
	want := "-Users-me-Sites-clyde-dev-clyde"
	if got != want {
		t.Fatalf("mangleProjectPath = %q, want %q", got, want)
	}
}

func TestLoadClaudeProjectMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := "/Users/me/Sites/app"
	dir := filepath.Join(home, ".claude", "projects", "-Users-me-Sites-app", "memory")
	writeMemoryFile(t, dir, "MEMORY.md", "# index\n- alpha")
	writeMemoryFile(t, dir, "alpha.md", "alpha body")

	docs, err := Load(providerid.ProviderClaude, workspace)
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs len = %d, want 2", len(docs))
	}
	if !docs[0].Index || docs[0].Title != "MEMORY" {
		t.Fatalf("first doc = %#v, want the MEMORY index first", docs[0])
	}
	if docs[1].Title != "alpha" || docs[1].Body != "alpha body" {
		t.Fatalf("second doc = %#v, want alpha body", docs[1])
	}
	if docs[0].Provider != "claude" {
		t.Fatalf("provider = %q, want claude", docs[0].Provider)
	}
}

func TestLoadCodexGlobalMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex", "memories")
	writeMemoryFile(t, dir, "MEMORY.md", "codex index")
	writeMemoryFile(t, dir, "raw_memories.md", "codex raw")

	docs, err := Load(providerid.ProviderCodex, "/any/workspace")
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs len = %d, want 2", len(docs))
	}
	if !docs[0].Index {
		t.Fatalf("first codex doc not the index: %#v", docs[0])
	}
	if docs[0].Provider != "codex" {
		t.Fatalf("provider = %q, want codex", docs[0].Provider)
	}
}

func TestLoadMissingDirReturnsNoDocs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	docs, err := Load(providerid.ProviderClaude, "/Users/me/never")
	if err != nil {
		t.Fatalf("missing dir err = %v, want nil", err)
	}
	if docs != nil {
		t.Fatalf("missing dir docs = %#v, want nil", docs)
	}
}

func TestLoadUnsupportedProvider(t *testing.T) {
	t.Parallel()
	docs, err := Load(providerid.ProviderArtifact, "/x")
	if err != nil {
		t.Fatalf("unsupported provider err = %v, want nil", err)
	}
	if docs != nil {
		t.Fatalf("unsupported provider docs = %#v, want nil", docs)
	}
}

func writeMemoryFile(t *testing.T, dir string, name string, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
