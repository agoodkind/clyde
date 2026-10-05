package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/sandbox"
)

const (
	rawTextReadableSession   = "raw-text-readable-session"
	rawTextUnreadableSession = "raw-text-unreadable-session"
	rawTextSearchTimeout     = 30 * time.Second
	rawTextPollInterval      = 200 * time.Millisecond
	rawTextDaemonConfig      = `[conversation.semantic]
ingestion_enabled = false
search_enabled = false

[adapter]
enabled = false

[mitm]
enabled_default = false
`
)

func writeRawTextTranscript(t *testing.T, path string, session string, minute int) {
	t.Helper()
	record := func(index int, role string, body string) string {
		head := fmt.Sprintf(`"sessionId":%q,"cwd":"/repo","uuid":"%02d%06d-0000-4000-8000-000000000000","timestamp":"2026-07-01T12:%02d:%02dZ"`, session, minute, index, minute, index)
		if role == "user" {
			return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},%s}`, body, head)
		}
		return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},%s}`, body, head)
	}
	lines := []string{
		record(0, "user", "why does the listener rebind on reload"),
		record(1, "assistant", "The Watcher classifies the CONFIG change before the daemon rebinds."),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

// startRawTextDaemon runs the daemon in this process with every Clyde path and
// every provider store under temporary directories. It returns the Claude
// project directory.
func startRawTextDaemon(t *testing.T) string {
	t.Helper()
	roots, err := sandbox.NewRoots()
	if err != nil {
		t.Fatalf("create sandbox roots: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(roots.Base) })
	for _, variable := range sandbox.Env(roots) {
		t.Setenv(variable.Name, variable.Value)
	}
	home := filepath.Join(roots.Base, "home")
	projectDir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}
	writeRawTextTranscript(t, filepath.Join(projectDir, rawTextReadableSession+".jsonl"), rawTextReadableSession, 0)
	writeRawTextTranscript(t, filepath.Join(projectDir, rawTextUnreadableSession+".jsonl"), rawTextUnreadableSession, 1)
	t.Setenv("HOME", home)
	for _, name := range []string{"CODEX_HOME", "CODEX_SQLITE_HOME", "CLYDE_CURSOR_PROJECTS_DIRS", "CLYDE_CURSOR_DATA_DIRS", "CLYDE_ZED_DATA_DIRS", "COPILOT_HOME"} {
		directory := filepath.Join(roots.Base, "providers", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
		t.Setenv(name, directory)
	}
	configPath := filepath.Join(roots.Config, "clyde", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(rawTextDaemonConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.RunContext(ctx, slog.Default())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case runErr := <-done:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("daemon run: %v", runErr)
			}
		case <-time.After(rawTextSearchTimeout):
			t.Error("daemon did not stop")
		}
	})
	return projectDir
}

func searchUntilMatches(t *testing.T, options conversation.SearchConversationsOptions, want int) conversation.SearchConversationsResult {
	t.Helper()
	var result conversation.SearchConversationsResult
	var searchErr error
	deadline := time.Now().Add(rawTextSearchTimeout)
	for time.Now().Before(deadline) {
		result, searchErr = daemon.SearchConversations(context.Background(), options)
		if searchErr == nil && len(result.Matches) == want {
			return result
		}
		time.Sleep(rawTextPollInterval)
	}
	t.Fatalf("SearchConversations returned %d matches and error %v, want %d matches", len(result.Matches), searchErr, want)
	return result
}

func TestSearchFallsBackToRawTextWhenSemanticSearchIsDisabled(t *testing.T) {
	projectDir := startRawTextDaemon(t)
	options := conversation.SearchConversationsOptions{Query: "watcher config", Limit: 5}

	searchUntilMatches(t, options, 2)

	// A read of a directory fails for every user, including root. A file with
	// mode 000 stays readable for root.
	unreadable := filepath.Join(projectDir, rawTextUnreadableSession+".jsonl")
	if err := os.Remove(unreadable); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatalf("replace transcript with a directory: %v", err)
	}

	result := searchUntilMatches(t, options, 1)
	if result.Source != conversation.SearchSourceRawText {
		t.Fatalf("source = %v, want raw_text", result.Source)
	}
	match := result.Matches[0]
	if match.Record.NativeID != rawTextReadableSession {
		t.Fatalf("match conversation = %q, want %q", match.Record.NativeID, rawTextReadableSession)
	}
	if match.MessageIndex != 1 || match.Role != "assistant" {
		t.Fatalf("match = index %d role %q, want index 1 role assistant", match.MessageIndex, match.Role)
	}
	if !strings.Contains(match.Snippet, "classifies the CONFIG change") {
		t.Fatalf("snippet = %q, want the matched assistant text", match.Snippet)
	}
	if result.HasMore {
		t.Fatal("HasMore = true, want false")
	}

	options.Cursor = "opaque-cursor"
	if _, err := daemon.SearchConversations(context.Background(), options); err == nil || !strings.Contains(err.Error(), "conversation_search_disabled") {
		t.Fatalf("SearchConversations with a cursor returned %v, want the disabled search error", err)
	}
}
