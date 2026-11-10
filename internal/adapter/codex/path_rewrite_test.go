package codex

import (
	"errors"
	"testing"
)

func withGetwd(t *testing.T, cwd string, err error, fn func()) {
	t.Helper()
	prev := GetwdFn
	GetwdFn = func() (string, error) { return cwd, err }
	t.Cleanup(func() { GetwdFn = prev })
	fn()
}

func TestRewriteWorkspacePathBailsOnRootCwd(t *testing.T) {
	withGetwd(t, "/", nil, func() {
		text := "see /Users/agoodkind/foo and /tmp/bar"
		got := rewriteWorkspacePath(text, "/Users/agoodkind/Sites/clyde-dev/clyde")
		if got != text {
			t.Errorf("expected text unchanged when cwd is `/`, got: %q", got)
		}
	})
}

func TestRewriteWorkspacePathBailsOnEmptyOrTrivialCwd(t *testing.T) {
	for _, cwd := range []string{"", "  ", "/", "//", "///"} {
		withGetwd(t, cwd, nil, func() {
			text := "see /Users/agoodkind/foo"
			got := rewriteWorkspacePath(text, "/Users/agoodkind/work")
			if got != text {
				t.Errorf("cwd=%q: expected text unchanged, got: %q", cwd, got)
			}
		})
	}
}

func TestRewriteWorkspacePathBailsOnGetwdError(t *testing.T) {
	withGetwd(t, "/whatever", errors.New("boom"), func() {
		text := "see /Users/agoodkind/foo"
		got := rewriteWorkspacePath(text, "/Users/agoodkind/work")
		if got != text {
			t.Errorf("expected text unchanged when GetwdFn errors, got: %q", got)
		}
	})
}

func TestRewriteWorkspacePathBailsWhenCwdEqualsWorkspace(t *testing.T) {
	withGetwd(t, "/Users/agoodkind/work", nil, func() {
		text := "see /Users/agoodkind/work/foo"
		got := rewriteWorkspacePath(text, "/Users/agoodkind/work")
		if got != text {
			t.Errorf("expected text unchanged when cwd==workspace, got: %q", got)
		}
	})
}

func TestRewriteWorkspacePathReplacesBoundedMatch(t *testing.T) {
	withGetwd(t, "/tmp/clyde-ws", nil, func() {
		text := "/tmp/clyde-ws/scratch.txt and /tmp/clyde-ws/notes.txt"
		got := rewriteWorkspacePath(text, "/Users/agoodkind/work")
		want := "/Users/agoodkind/work/scratch.txt and /Users/agoodkind/work/notes.txt"
		if got != want {
			t.Errorf("expected:\n  %q\ngot:\n  %q", want, got)
		}
	})
}

func TestRewriteWorkspacePathDoesNotMashUnboundedSubstring(t *testing.T) {
	withGetwd(t, "/var", nil, func() {
		// /var should not match inside /variable or /vary
		text := "the /variable holds /var/log/foo"
		got := rewriteWorkspacePath(text, "/work")
		want := "the /variable holds /work/log/foo"
		if got != want {
			t.Errorf("expected:\n  %q\ngot:\n  %q", want, got)
		}
	})
}
