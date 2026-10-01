package parser

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestReadContextWindowsReadsOneFreshCursorStream(t *testing.T) {
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
	virtual := BuildVirtualPath(RootHash(root), VirtualKindComposer, composerOnlyID)
	windows := []conversation.ContextMessageWindow{{Start: 1, End: 2}, {Start: 0, End: 2}, {Start: 0, End: 1}}
	options := conversation.LoadOptions{}
	stats, err := New().ReadContextWindows(t.Context(), virtual, "", windows, options, func(selected [][]transcript.Message) error {
		if len(selected) != 3 || len(selected[0]) != 1 || len(selected[1]) != 2 || len(selected[2]) != 1 {
			t.Fatal("fresh Cursor windows have incorrect positions")
		}
		if selected[1][0].Text != "composer question" || selected[2][0].Text != selected[1][0].Text || selected[0][0].Text != selected[1][1].Text {
			t.Fatal("fresh Cursor windows have incorrect source content")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.SourceReads != 1 || stats.MessagesRetained != 2 || stats.MessagesVisited != 2 || stats.Windows != 3 {
		t.Fatalf("Cursor fresh stream statistics differ: %+v", stats)
	}
	stats, err = New().ReadContextWindows(t.Context(), virtual, "", windows, options, func([][]transcript.Message) error {
		executeCursorContextFixture(t, writer, "context-edit.sql")
		return nil
	})
	if err == nil || stats.SourceReads != 1 {
		t.Fatalf("Cursor accepted mutation during plural verification: %+v, %v", stats, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = New().ReadContextWindows(ctx, virtual, "", windows, options, func([][]transcript.Message) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Cursor plural cancellation returned %v", err)
	}
}
