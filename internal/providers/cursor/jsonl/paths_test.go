package cursorjsonl

import (
	"os"
	"path/filepath"
	"testing"
)

const testConversationID = "123e4567-e89b-12d3-a456-426614174000"

func TestResolveProjectRootsUsesEnvOverride(t *testing.T) {
	firstRoot := filepath.Join(t.TempDir(), "first")
	secondRoot := filepath.Join(t.TempDir(), "second")
	t.Setenv(cursorProjectsDirsEnvVar, firstRoot+string(os.PathListSeparator)+secondRoot)

	roots, err := ResolveProjectRoots()
	if err != nil {
		t.Fatalf("ResolveProjectRoots returned error: %v", err)
	}

	if len(roots) != 2 {
		t.Fatalf("ResolveProjectRoots returned %d roots, want 2", len(roots))
	}
	if roots[0].Path != firstRoot {
		t.Fatalf("first root path = %q, want %q", roots[0].Path, firstRoot)
	}
	if roots[1].Path != secondRoot {
		t.Fatalf("second root path = %q, want %q", roots[1].Path, secondRoot)
	}
}

func TestDiscoverTranscriptFilesFindsMatchingTranscriptFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	projectKey := "Users-agoodkind-Sites-configs"
	transcriptPath := filepath.Join(
		root,
		projectKey,
		"agent-transcripts",
		testConversationID,
		testConversationID+".jsonl",
	)
	writeTestFile(t, transcriptPath, `{"role":"user","message":{"content":[]}}`+"\n")

	writeTestFile(t, filepath.Join(root, projectKey, "agent-transcripts", testConversationID, "junk.jsonl"), "{}\n")
	writeTestFile(t, filepath.Join(root, projectKey, "agent-transcripts", "other", "other.txt"), "{}\n")
	writeTestFile(t, filepath.Join(root, projectKey, "other", testConversationID, testConversationID+".jsonl"), "{}\n")

	files, err := DiscoverTranscriptFiles([]ProjectRoot{{Path: root}})
	if err != nil {
		t.Fatalf("DiscoverTranscriptFiles returned error: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("DiscoverTranscriptFiles returned %d files, want 1: %#v", len(files), files)
	}
	if files[0].Path != transcriptPath {
		t.Fatalf("Path = %q, want %q", files[0].Path, transcriptPath)
	}
	if files[0].ConversationID != testConversationID {
		t.Fatalf("ConversationID = %q, want %q", files[0].ConversationID, testConversationID)
	}
	if files[0].ProjectKey != projectKey {
		t.Fatalf("ProjectKey = %q, want %q", files[0].ProjectKey, projectKey)
	}
}

func TestMatchTranscriptFileRejectsPathOutsideConfiguredRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) returned error: %v", root, err)
	}
	t.Setenv(cursorProjectsDirsEnvVar, root)

	outsidePath := filepath.Join(
		filepath.Dir(root),
		agentTranscriptsDirName,
		testConversationID,
		testConversationID+".jsonl",
	)
	writeTestFile(t, outsidePath, `{"role":"user","message":{"content":[]}}`+"\n")

	file, ok, err := MatchTranscriptFile(outsidePath)
	if err != nil {
		t.Fatalf("MatchTranscriptFile returned error: %v", err)
	}
	if ok {
		t.Fatalf("MatchTranscriptFile(%q) = %#v, true, want no match", outsidePath, file)
	}
}

func TestWorkspacePathFromProjectKeyReturnsLossyDisplayHint(t *testing.T) {
	t.Parallel()

	got := WorkspacePathFromProjectKey("Users-agoodkind-Sites-configs")
	want := "/Users/agoodkind/Sites/configs"
	if got != want {
		t.Fatalf("WorkspacePathFromProjectKey() = %q, want %q", got, want)
	}
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) returned error: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) returned error: %v", path, err)
	}
}
