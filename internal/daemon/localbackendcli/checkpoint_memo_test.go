package localbackendcli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	memoBatchWorkspace       = "/checkpoint/memo"
	memoBatchChangedSession  = "memo-batch-a"
	memoBatchBlockedSession  = "memo-batch-b"
	memoBatchFolderMode      = 0o700
	memoBatchCheckpointChars = 32
	memoBatchPollDelay       = 100 * time.Millisecond
	memoBatchWaitLimit       = 5 * time.Minute
	memoBatchPassEvent       = "daemon.conversation_semantic_sync.pass_completed"
	memoBatchFreshnessError  = "semantic_freshness.error"
	memoBatchMessageTime     = "2026-05-02T10:00:00Z"
	memoBatchFirstAge        = 6 * time.Hour
	memoBatchSecondAge       = 5 * time.Hour
	memoBatchThirdAge        = 4 * time.Hour
	memoBatchContentA        = "content A about the harbor survey"
	memoBatchContentB        = "content B about the harbor survey"
	memoBatchOtherContent    = "other conversation about the harbor survey"
	memoBatchOtherChanged    = "changed other conversation about the harbor survey"
)

type memoBatchPassRecord struct {
	Message         string `json:"msg"`
	Failed          int    `json:"failed"`
	UnchangedPinned int    `json:"unchanged_pinned"`
}

type memoBatchFixture struct {
	t      *testing.T
	home   string
	daemon *localProofDaemon
}

func memoBatchLine(session string, text string) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},"sessionId":%q,"cwd":%q,"uuid":"00000000-0000-4000-8000-%012x","timestamp":%q}`+"\n",
		text, session, memoBatchWorkspace, len(session), memoBatchMessageTime)
}

// install writes a temporary transcript and renames it to the project transcript path.
// It preserves the project folder and the other transcript.
func (f *memoBatchFixture) install(session string, text string, age time.Duration) {
	f.t.Helper()
	project := filepath.Join(f.home, ".claude", "projects", strings.ReplaceAll(memoBatchWorkspace, "/", "-"))
	if err := os.MkdirAll(project, memoBatchFolderMode); err != nil {
		f.t.Fatalf("The test could not create the transcript folder: %v.", err)
	}
	modified := time.Now().Add(-age)
	temporary := filepath.Join(f.home, session+".tmp")
	if err := os.WriteFile(temporary, []byte(memoBatchLine(session, text)), 0o600); err != nil {
		f.t.Fatalf("The test could not write the conversation transcript: %v.", err)
	}
	if err := os.Chtimes(temporary, modified, modified); err != nil {
		f.t.Fatalf("The test could not set the transcript file times: %v.", err)
	}
	if err := os.Rename(temporary, filepath.Join(project, session+".jsonl")); err != nil {
		f.t.Fatalf("The test could not replace the project transcript with the temporary file: %v.", err)
	}
}

func (f *memoBatchFixture) statusCounter(name string) any {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, f.daemon.binaryPath, "status", "--output-format", "json")
	command.Env = f.daemon.environment
	var commandStderr bytes.Buffer
	command.Stderr = &commandStderr
	output, err := command.Output()
	if err != nil {
		f.t.Fatalf("The daemon status command failed: %v. The command wrote this stderr output: %s", err, commandStderr.String())
	}
	var status struct {
		Counters map[string]struct {
			Value any `json:"value"`
		} `json:"counters"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		f.t.Fatalf("The test could not decode the daemon status: %v.", err)
	}
	if failure, failed := status.Counters[memoBatchFreshnessError]; failed {
		f.t.Fatalf("The daemon reported %s in its status output: %v.", memoBatchFreshnessError, failure.Value)
	}
	return status.Counters[name].Value
}

func (f *memoBatchFixture) waitFor(description string, check func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(memoBatchWaitLimit)
	for !check() {
		if time.Now().After(deadline) {
			f.t.Fatalf("The test timed out while waiting for %s. The daemon wrote this stderr output: %s", description, f.daemon.stderr.String())
		}
		time.Sleep(memoBatchPollDelay)
	}
}

func memoBatchCheckpointName(session string) string {
	digest := sha256.Sum256([]byte(localProofProviderClaude + ":" + session))
	return hex.EncodeToString(digest[:])[:memoBatchCheckpointChars] + ".json"
}

func (f *memoBatchFixture) checkpointPath(session string) string {
	f.t.Helper()
	pattern := filepath.Join(f.daemon.roots.State, "clyde", "conversation-semantic", "checkpoints", "*", memoBatchCheckpointName(session))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		f.t.Fatalf("The test could not search for the checkpoint: %v.", err)
	}
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func (f *memoBatchFixture) checkpoint(session string) string {
	f.t.Helper()
	path := f.checkpointPath(session)
	if path == "" {
		return ""
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(content)
}

func (f *memoBatchFixture) noChatsNeeded() bool {
	needed, isNumber := f.statusCounter("semantic_freshness.needed").(float64)
	return isNumber && needed == 0
}

// passRecords reads and deduplicates pass records from both daemon state roots.
func (f *memoBatchFixture) passRecords() []memoBatchPassRecord {
	f.t.Helper()
	records := []memoBatchPassRecord{}
	seen := map[string]bool{}
	for _, stateRoot := range []string{filepath.Join(f.home, ".local", "state"), f.daemon.roots.State} {
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
				if !strings.HasSuffix(line, "\n") || !strings.Contains(line, memoBatchPassEvent) || seen[line] {
					continue
				}
				seen[line] = true
				var record memoBatchPassRecord
				if decodeErr := json.Unmarshal([]byte(line), &record); decodeErr != nil {
					return decodeErr
				}
				if record.Message == memoBatchPassEvent {
					records = append(records, record)
				}
			}
			return nil
		})
		if walkErr != nil {
			f.t.Fatalf("The test could not read the daemon pass records: %v.", walkErr)
		}
	}
	return records
}

func (f *memoBatchFixture) failedPassCount() int {
	f.t.Helper()
	count := 0
	for _, record := range f.passRecords() {
		if record.Failed > 0 {
			count++
		}
	}
	return count
}

// TestLocalFeederResendsChatAfterBatchFailureAfterCheckpoint checks retries with a local
// backend daemon running through the sandbox command. The first conversation has a
// deliveredContent memo for content A. A batch saves its checkpoint for content B,
// then fails at the second conversation's checkpoint. The first conversation then
// returns to content A with a new file time. The test checks the daemon log for the
// failed batch and the resend of content A.
// It rejects conversation.vectorsearch.upsert_completed records that list rows from
// the failed batch.
func TestLocalFeederResendsChatAfterBatchFailureAfterCheckpoint(t *testing.T) {
	fixture := &memoBatchFixture{t: t, home: t.TempDir(), daemon: nil}
	fixture.install(memoBatchBlockedSession, memoBatchOtherContent, memoBatchFirstAge)
	fixture.install(memoBatchChangedSession, memoBatchContentA, memoBatchFirstAge)
	settingsPath := filepath.Join(t.TempDir(), "semantic-settings.toml")
	writeLocalProofFile(t, settingsPath, sandboxFastIntervals)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	fixture.daemon = startLocalProofDaemon(t, buildClydeCLI(t), fixture.home, "--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	fixture.waitFor("the first delivery", func() bool {
		return fixture.checkpoint(memoBatchChangedSession) != "" && fixture.checkpoint(memoBatchBlockedSession) != "" && fixture.noChatsNeeded()
	})
	checkpointA := fixture.checkpoint(memoBatchChangedSession)

	// A nonempty directory prevents the second conversation's checkpoint rename.
	blockerPath := fixture.checkpointPath(memoBatchBlockedSession)
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("The test could not remove the second conversation's checkpoint file: %v.", err)
	}
	if err := os.MkdirAll(filepath.Join(blockerPath, "blocker"), memoBatchFolderMode); err != nil {
		t.Fatalf("The test could not create the directory that blocks the checkpoint: %v.", err)
	}
	// The daemon keeps retrying the blocked conversation.
	// A later edit adds the first conversation to a batch with that retry.
	failedBatchStart := time.Now()
	fixture.install(memoBatchBlockedSession, memoBatchOtherChanged, memoBatchSecondAge)
	fixture.waitFor("the failed batch", func() bool {
		return fixture.failedPassCount() > 0
	})
	// The failed batch writes rows for the blocked conversation. A completion record with
	// those rows would report success for a batch that failed.
	blockedRows := localProofProviderClaude + ":" + memoBatchBlockedSession + "="
	for _, record := range readLocalProofUpserts(t, fixture.home, fixture.daemon.roots, failedBatchStart) {
		listsBlockedRows := slices.ContainsFunc(record.WrittenConversations, func(written string) bool { return strings.HasPrefix(written, blockedRows) })
		if listsBlockedRows {
			t.Fatalf("The daemon log reports %s for a batch that failed at its checkpoint: %v.", localProofUpsertEvent, record.WrittenConversations)
		}
	}
	fixture.install(memoBatchChangedSession, memoBatchContentB, memoBatchSecondAge)
	checkpointB := ""
	fixture.waitFor("the checkpoint of content B", func() bool {
		checkpointB = fixture.checkpoint(memoBatchChangedSession)
		return checkpointB != checkpointA
	})
	// The batch saves checkpoint B before the blocked conversation attempts its checkpoint.
	// A new failed pass proves that the blocked checkpoint failed after checkpoint B.
	failedPassCount := fixture.failedPassCount()
	fixture.waitFor("a failed pass after the checkpoint of content B", func() bool {
		return fixture.failedPassCount() > failedPassCount
	})

	if err := os.RemoveAll(blockerPath); err != nil {
		t.Fatalf("The test could not remove the directory that blocks the checkpoint: %v.", err)
	}
	fixture.install(memoBatchChangedSession, memoBatchContentA, memoBatchThirdAge)
	fixture.waitFor("the delivery of content A", func() bool {
		if slices.ContainsFunc(fixture.passRecords(), func(record memoBatchPassRecord) bool { return record.UnchangedPinned > 0 }) {
			t.Fatalf("The worker skipped the first conversation at content A while its checkpoint recorded content B.")
		}
		current := fixture.checkpoint(memoBatchChangedSession)
		return current != checkpointA && current != checkpointB && fixture.noChatsNeeded()
	})
}
