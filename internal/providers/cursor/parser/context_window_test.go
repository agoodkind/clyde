package parser

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestReadContextWindowReloadsCursorAndRejectsMissingHeader(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", root)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	createCursorParserGlobalDB(t, path)
	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	virtual := "cursor://" + RootHash(root) + "/composer/" + composerOnlyID
	found := false
	for _, candidate := range candidates {
		if candidate.Path == virtual {
			found = true
		}
	}
	if !found {
		t.Fatal("actual composer was not discovered")
	}
	options := conversation.LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: false, IncludeToolOutputs: false}
	prior, err := conversation.CollectMessages(parser.Stream(virtual, options))
	if err != nil || len(prior) != 2 || prior[0].Text != "composer question" {
		t.Fatalf("initial composer: %+v, %v", prior, err)
	}
	writer, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	executeCursorContextFixture(t, writer, "context-edit.sql")
	err = parser.ReadContextWindow(t.Context(), virtual, "", 0, 2, options, func(messages []transcript.Message) error {
		if len(messages) != 2 || messages[0].Text != "changed composer question" {
			t.Fatalf("context returned stale source: %+v", messages)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fresh composer context: %v", err)
	}
	err = parser.ReadContextWindow(t.Context(), virtual, "", 0, 2, options, func([]transcript.Message) error {
		executeCursorContextFixture(t, writer, "context-delete-header.sql")
		return nil
	})
	if err == nil {
		t.Fatal("context accepted an edit during verification")
	}
	err = parser.ReadContextWindow(t.Context(), virtual, "", 0, 2, options, func([]transcript.Message) error {
		t.Fatal("context used the cached header after its source row was deleted")
		return nil
	})
	if err == nil {
		t.Fatal("context accepted an absent composer header")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	err = parser.ReadContextWindow(cancelled, virtual, "", 0, 2, options, func([]transcript.Message) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation returned %v", err)
	}
}

func executeCursorContextFixture(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), string(content)); err != nil {
		t.Fatal(err)
	}
}

func TestReadContextWindowReadsCursorAfterAllWritersClose(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", root)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	createCursorParserGlobalDB(t, path)
	writer, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	executeCursorContextFixture(t, writer, "context-edit.sql")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("closed writer retained %s: %v", suffix, err)
		}
	}
	options := conversation.LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: false, IncludeToolOutputs: false}
	virtual := BuildVirtualPath(RootHash(root), VirtualKindComposer, composerOnlyID)
	err = New().ReadContextWindow(t.Context(), virtual, "", 0, 2, options, func(messages []transcript.Message) error {
		if len(messages) != 2 || messages[0].Text != "changed composer question" {
			t.Fatalf("closed-writer context returned stale source: %+v", messages)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("context rejected its own temporary sidecars: %v", err)
	}
}
