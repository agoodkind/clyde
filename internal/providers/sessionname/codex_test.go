package sessionname

import (
	"context"
	"testing"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/session"
)

func TestRenameDispatchesToCodexWriter(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)

	sess := &session.Session{
		Name: "codex-chat",
		Metadata: session.Metadata{
			Provider:  session.ProviderCodex,
			SessionID: "thread-1",
		},
	}

	if err := Rename(context.Background(), sess, "Renamed Codex"); err != nil {
		t.Fatalf("Rename returned error: %v", err)
	}

	paths, err := codexstore.ResolveStorePaths(codexHome, codexHome)
	if err != nil {
		t.Fatalf("ResolveStorePaths: %v", err)
	}
	idx, err := codexstore.ReadSessionIndex(paths.SessionIndexPath)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if got := idx.ThreadName("thread-1"); got != "Renamed Codex" {
		t.Fatalf("ThreadName = %q, want Renamed Codex", got)
	}
}

func TestRenameCodexRejectsMissingSessionID(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)
	sess := &session.Session{
		Name: "codex-chat",
		Metadata: session.Metadata{
			Provider: session.ProviderCodex,
		},
	}
	if err := renameCodex(sess, "Renamed Codex"); err == nil {
		t.Fatal("renameCodex(missing session id) returned nil error")
	}
}

func TestRenameCodexAppendsNormalizedThreadName(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)

	sess := &session.Session{
		Name: "codex-chat",
		Metadata: session.Metadata{
			Provider:  session.ProviderCodex,
			SessionID: "thread-42",
		},
	}
	if err := renameCodex(sess, "  Renamed Codex  "); err != nil {
		t.Fatalf("renameCodex returned error: %v", err)
	}

	paths, err := codexstore.ResolveStorePaths(codexHome, codexHome)
	if err != nil {
		t.Fatalf("ResolveStorePaths: %v", err)
	}
	idx, err := codexstore.ReadSessionIndex(paths.SessionIndexPath)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if got := idx.ThreadName("thread-42"); got != "Renamed Codex" {
		t.Fatalf("ThreadName = %q, want Renamed Codex", got)
	}
}

// TestRenameCodexRejectsEmptyNormalizedName covers the path where a caller
// bypasses Rename and hands renameCodex a whitespace-only title. The
// underlying NormalizeThreadName must reject it so renameCodex never
// writes a thread name with no visible characters.
func TestRenameCodexRejectsEmptyNormalizedName(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)

	sess := &session.Session{
		Name: "codex-chat",
		Metadata: session.Metadata{
			Provider:  session.ProviderCodex,
			SessionID: "thread-42",
		},
	}
	if err := renameCodex(sess, "   "); err == nil {
		t.Fatal("renameCodex(whitespace) returned nil error")
	}
}
