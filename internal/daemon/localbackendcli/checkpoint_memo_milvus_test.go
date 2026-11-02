//go:build milvusintegration

package localbackendcli_test

import (
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

	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	memoBatchWorkspace       = "/checkpoint/memo"
	memoBatchChangedSession  = "memo-batch-a"
	memoBatchBlockedSession  = "memo-batch-b"
	memoBatchFolderMode      = 0o700
	memoBatchCheckpointChars = 32
	memoBatchPollDelay       = 100 * time.Millisecond
	memoBatchWaitLimit       = 10 * time.Minute
	memoBatchPassEvent       = "daemon.conversation_semantic_sync.pass_completed"
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
	t              *testing.T
	home           string
	daemon         *localProofDaemon
	collectionName string
	version        int
}

func memoBatchLine(session string, text string, modified time.Time) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},"sessionId":%q,"cwd":%q,"uuid":"00000000-0000-4000-8000-%012x","timestamp":%q}`+"\n",
		text, session, memoBatchWorkspace, len(session), modified.Format(time.RFC3339))
}

func (f *memoBatchFixture) projectFolder() string {
	return filepath.Join(f.home, ".claude", "projects", strings.ReplaceAll(memoBatchWorkspace, "/", "-"))
}

// install replaces the project folder so a scan can read both conversations.
func (f *memoBatchFixture) install(changedText string, otherText string, changedAge time.Duration, otherAge time.Duration) {
	f.t.Helper()
	f.version++
	temporary := filepath.Join(f.home, fmt.Sprintf("memo-batch-%d", f.version))
	if err := os.MkdirAll(temporary, memoBatchFolderMode); err != nil {
		f.t.Fatalf("The test could not create the transcript folder: %v.", err)
	}
	f.writeChat(temporary, memoBatchChangedSession, changedText, changedAge)
	f.writeChat(temporary, memoBatchBlockedSession, otherText, otherAge)
	project := f.projectFolder()
	if err := os.MkdirAll(filepath.Dir(project), memoBatchFolderMode); err != nil {
		f.t.Fatalf("The test could not create the parent projects folder: %v.", err)
	}
	previous := fmt.Sprintf("%s-previous-%d", temporary, f.version)
	if err := os.Rename(project, previous); err != nil && !errors.Is(err, fs.ErrNotExist) {
		f.t.Fatalf("The test could not rename the existing transcript folder: %v.", err)
	}
	if err := os.Rename(temporary, project); err != nil {
		f.t.Fatalf("The test could not rename the replacement transcript folder into place: %v.", err)
	}
}

func (f *memoBatchFixture) writeChat(folder string, session string, text string, age time.Duration) {
	f.t.Helper()
	modified := time.Now().Add(-age)
	path := filepath.Join(folder, session+".jsonl")
	if err := os.WriteFile(path, []byte(memoBatchLine(session, text, modified)), 0o600); err != nil {
		f.t.Fatalf("The test could not write the conversation transcript: %v.", err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		f.t.Fatalf("The test could not set the transcript file times: %v.", err)
	}
}

func (f *memoBatchFixture) statusCounter(name string) any {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, f.daemon.binaryPath, "status", "--output-format", "json")
	command.Env = f.daemon.environment
	output, err := command.Output()
	if err != nil {
		return nil
	}
	var status struct {
		Counters map[string]struct {
			Value any `json:"value"`
		} `json:"counters"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		f.t.Fatalf("The test could not decode the daemon status: %v.", err)
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

func (f *memoBatchFixture) checkpointPath(session string) string {
	digest := sha256.Sum256([]byte(localProofProviderClaude + ":" + session))
	name := hex.EncodeToString(digest[:])[:memoBatchCheckpointChars] + ".json"
	return filepath.Join(f.daemon.roots.State, "clyde", "conversation-semantic", "checkpoints", f.collectionName, name)
}

func (f *memoBatchFixture) checkpoint(session string) string {
	content, err := os.ReadFile(f.checkpointPath(session))
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

// TestMilvusFeederResendsChatAfterBatchFailureAfterCheckpoint checks retries with Milvus.
// The first conversation has a delivery memo for content A. A batch saves its checkpoint
// for content B before the second checkpoint fails. The conversation then returns to
// content A with a new file time, and the daemon must send content A again.
func TestMilvusFeederResendsChatAfterBatchFailureAfterCheckpoint(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	baseURL := os.Getenv(deletedSourceEmbeddingEnv)
	if baseURL == "" {
		baseURL = deletedSourceEmbeddingDefault
	}
	fixture := &memoBatchFixture{t: t, home: t.TempDir(), daemon: nil, collectionName: "", version: 0}
	fixture.install(memoBatchContentA, memoBatchOtherContent, memoBatchFirstAge, memoBatchFirstAge)
	settingsPath := filepath.Join(t.TempDir(), "semantic-settings.toml")
	settings := sandboxFastIntervals + fmt.Sprintf(withheldSettings, address, baseURL, deletedSourceEmbeddingModel, deletedSourceEmbeddingDim)
	writeLocalProofFile(t, settingsPath, settings)
	fixture.daemon = startLocalProofDaemon(t, buildClydeCLI(t), fixture.home, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	fixture.waitFor("the collection name", func() bool {
		name, isText := fixture.statusCounter("semantic_settings.collection_name").(string)
		fixture.collectionName = name
		return isText && name != ""
	})
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
	fixture.install(memoBatchContentB, memoBatchOtherChanged, memoBatchSecondAge, memoBatchSecondAge)
	checkpointB := ""
	fixture.waitFor("the checkpoint of content B and the failed batch", func() bool {
		checkpointB = fixture.checkpoint(memoBatchChangedSession)
		failed := slices.ContainsFunc(fixture.passRecords(), func(record memoBatchPassRecord) bool { return record.Failed > 0 })
		return checkpointB != checkpointA && failed
	})

	if err := os.RemoveAll(blockerPath); err != nil {
		t.Fatalf("The test could not remove the directory that blocks the checkpoint: %v.", err)
	}
	fixture.install(memoBatchContentA, memoBatchOtherChanged, memoBatchThirdAge, memoBatchSecondAge)
	fixture.waitFor("the delivery of content A", func() bool {
		if slices.ContainsFunc(fixture.passRecords(), func(record memoBatchPassRecord) bool { return record.UnchangedPinned > 0 }) {
			t.Fatalf("The worker skipped the first conversation at content A while its checkpoint recorded content B.")
		}
		current := fixture.checkpoint(memoBatchChangedSession)
		return current != checkpointA && current != checkpointB && fixture.noChatsNeeded()
	})
}
