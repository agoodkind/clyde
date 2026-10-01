//go:build darwin || linux

package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestReadContextWindowRejectsReplacedAdmittedDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	root := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", root)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	createCursorParserGlobalDB(t, path)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	executeCursorContextFixture(t, writer, "context-edit.sql")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	options := conversation.LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: false, IncludeToolOutputs: false}
	virtual := BuildVirtualPath(RootHash(root), VirtualKindComposer, composerOnlyID)
	replacements := 0
	rejections := 0
	for attempt := 0; attempt < 16384; attempt++ {
		if err := ctx.Err(); err != nil {
			t.Fatalf("replacement exercise exceeded its deadline: %v", err)
		}
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		replacementPath := filepath.Join(root, "replacement.vscdb")
		if err := os.WriteFile(replacementPath, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
		prior, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		result := make(chan error, 1)
		go replaceAdmittedCursorDatabase(path, replacementPath, prior, time.Duration(attempt%128)*time.Microsecond, done, result)
		stale := false
		err = New().ReadContextWindow(ctx, virtual, "", 0, 2, options, func(messages []transcript.Message) error {
			current, statErr := os.Stat(path)
			if statErr != nil {
				return statErr
			}
			stale = !os.SameFile(prior, current) && len(messages) == 2 && messages[0].Text == "composer question"
			return nil
		})
		close(done)
		if replaceErr := <-result; replaceErr != nil {
			t.Fatal(replaceErr)
		}
		current, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if !os.SameFile(prior, current) {
			replacements++
			if err != nil {
				rejections++
			}
		}
		if err == nil && stale {
			t.Fatalf("attempt %d accepted content from the replaced admitted inode", attempt)
		}
	}
	if replacements == 0 || rejections == 0 {
		t.Fatalf("replacement exercise was incomplete: %d replacements, %d rejections", replacements, rejections)
	}
	t.Logf("actual one-way replacements: %d; rejected reads: %d", replacements, rejections)
	if err := New().ReadContextWindow(t.Context(), virtual, "", 0, 2, options, func(messages []transcript.Message) error {
		if len(messages) != 2 {
			t.Fatalf("stable context returned %d messages", len(messages))
		}
		return nil
	}); err != nil {
		t.Fatalf("stable context failed after replacements: %v", err)
	}
}

func replaceAdmittedCursorDatabase(path, replacement string, prior os.FileInfo, delay time.Duration, done <-chan struct{}, result chan<- error) {
	identity := prior.Sys().(*syscall.Stat_t)
	for {
		select {
		case <-done:
			result <- nil
			return
		default:
		}
		for descriptor := 0; descriptor < 256; descriptor++ {
			var current syscall.Stat_t
			if syscall.Fstat(descriptor, &current) != nil || current.Dev != identity.Dev || current.Ino != identity.Ino {
				continue
			}
			deadline := time.Now().Add(delay)
			for time.Now().Before(deadline) {
			}
			result <- os.Rename(replacement, path)
			return
		}
		runtime.Gosched()
	}
}
