package localbackendcli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	backlogFailureWorkspace    = "/backlog/failure"
	backlogFailureFirstSession = "backlog-failure-first"
	backlogFailureNextSession  = "backlog-failure-next"
	backlogFailureSettings     = "sync_interval = \"2s\"\nindex_refresh_interval = \"1s\"\n"
	backlogFailurePassEvent    = "daemon.conversation_semantic_sync.pass_completed"
	backlogFailureAge          = 2 * time.Minute
	backlogFailureWindow       = 6 * time.Second
	backlogFailureWaitLimit    = 5 * time.Minute
	backlogFailurePollDelay    = 100 * time.Millisecond
	backlogFailureNameChars    = 32
	backlogFailureReadOnlyMode = 0o500
	backlogFailureWritableMode = 0o700
	// backlogFailureMaxPasses limits failed passes during backlogFailureWindow.
	backlogFailureMaxPasses = 16
)

type backlogFailurePassRecord struct {
	Message string `json:"msg"`
	Failed  int    `json:"failed"`
}

func installBacklogFailureChat(t *testing.T, home string, session string) {
	t.Helper()
	modified := time.Now().Add(-backlogFailureAge)
	line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},"sessionId":%q,"cwd":%q,"uuid":"00000000-0000-4000-8000-%012x","timestamp":%q}`+"\n",
		"backlog failure record for "+session, session, backlogFailureWorkspace, len(session), modified.UTC().Format(time.RFC3339))
	path := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(backlogFailureWorkspace, "/", "-"), session+".jsonl")
	installLocalProofTranscript(t, path, line)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("The test could not set the transcript time: %v.", err)
	}
}

func backlogFailureCheckpointFolder(t *testing.T, daemon *localProofDaemon, session string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(localProofProviderClaude + ":" + session))
	name := hex.EncodeToString(digest[:])[:backlogFailureNameChars] + ".json"
	matches, err := filepath.Glob(filepath.Join(daemon.roots.State, "clyde", "conversation-semantic", "checkpoints", "*", name))
	if err != nil {
		t.Fatalf("The test could not find the checkpoint: %v.", err)
	}
	if len(matches) == 0 {
		return ""
	}
	return filepath.Dir(matches[0])
}

// daemonLogLines returns complete log lines containing event from both daemon
// state roots and removes duplicate lines.
func daemonLogLines(t *testing.T, home string, daemon *localProofDaemon, event string) []string {
	t.Helper()
	lines := []string{}
	seen := map[string]bool{}
	for _, stateRoot := range []string{filepath.Join(home, ".local", "state"), daemon.roots.State} {
		walkErr := filepath.WalkDir(stateRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if entry.IsDir() || entry.Name() != localProofDaemonLogName {
				return nil
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for line := range strings.Lines(string(content)) {
				if strings.HasSuffix(line, "\n") && strings.Contains(line, event) && !seen[line] {
					seen[line] = true
					lines = append(lines, line)
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("The test could not read daemon logs under %s: %v", stateRoot, walkErr)
		}
	}
	return lines
}

func backlogFailedPasses(t *testing.T, home string, daemon *localProofDaemon) int {
	t.Helper()
	failed := 0
	for _, line := range daemonLogLines(t, home, daemon, backlogFailurePassEvent) {
		var record backlogFailurePassRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("The test could not read the daemon logs: %v.", err)
		}
		if record.Message == backlogFailurePassEvent && record.Failed > 0 {
			failed++
		}
	}
	return failed
}

func waitForBacklogFailure(t *testing.T, daemon *localProofDaemon, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(backlogFailureWaitLimit)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("The test did not observe %s within the wait limit. The daemon wrote this stderr output: %s.", description, daemon.stderr.String())
		}
		time.Sleep(backlogFailurePollDelay)
	}
}

// TestDaemonBacklogStopsAfterFailedDeliveries runs the sandbox daemon with the
// local store. It makes the checkpoint folder read-only before installing a chat.
// It counts failed passes in the daemon log during backlogFailureWindow.
// The count must exceed zero and must not exceed backlogFailureMaxPasses.
func TestDaemonBacklogStopsAfterFailedDeliveries(t *testing.T) {
	home := t.TempDir()
	installBacklogFailureChat(t, home, backlogFailureFirstSession)
	settingsPath := filepath.Join(t.TempDir(), "semantic-settings.toml")
	writeLocalProofFile(t, settingsPath, backlogFailureSettings)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	daemon := startLocalProofDaemon(t, buildClydeCLI(t), home, "--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)

	folder := ""
	waitForBacklogFailure(t, daemon, "a checkpoint for the first chat", func() bool {
		folder = backlogFailureCheckpointFolder(t, daemon, backlogFailureFirstSession)
		return folder != ""
	})
	if err := os.Chmod(folder, backlogFailureReadOnlyMode); err != nil {
		t.Fatalf("The test could not make the checkpoint folder read-only: %v.", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(folder, backlogFailureWritableMode); err != nil {
			t.Errorf("The test could not make the checkpoint folder writable: %v.", err)
		}
	})
	installBacklogFailureChat(t, home, backlogFailureNextSession)
	waitForBacklogFailure(t, daemon, "a sync pass with a failed delivery", func() bool { return backlogFailedPasses(t, home, daemon) > 0 })

	before := backlogFailedPasses(t, home, daemon)
	time.Sleep(backlogFailureWindow)
	during := backlogFailedPasses(t, home, daemon) - before
	if during == 0 {
		t.Fatalf("The worker did not retry the failed delivery within %v.", backlogFailureWindow)
	}
	if during > backlogFailureMaxPasses {
		t.Fatalf("The worker ran %d failed passes within %v. The limit is %d.", during, backlogFailureWindow, backlogFailureMaxPasses)
	}
}
