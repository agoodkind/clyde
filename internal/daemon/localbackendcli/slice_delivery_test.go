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
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	sliceDeliveryWorkspace     = "/slice/delivery"
	sliceDeliveryLargeSession  = "slice-delivery-large"
	sliceDeliveryNewerSession  = "slice-delivery-newer"
	sliceDeliveryLargeMessages = 600
	sliceDeliveryNewerMessages = 2
	sliceDeliverySettings      = "ingest_slice_rows = 8\n"
	sliceDeliveryPassEvent     = "daemon.conversation_semantic_sync.pass_completed"
	sliceDeliveryNameChars     = 32
	sliceDeliveryFolderMode    = 0o700
	sliceDeliveryLargeAge      = 6 * time.Hour
	sliceDeliveryNewerAge      = 2 * time.Minute
	sliceDeliveryWaitLimit     = 10 * time.Minute
	sliceDeliveryPollDelay     = 100 * time.Millisecond
	sliceDeliveryClaimDelay    = time.Millisecond
	sliceDeliverySearchLimit   = 5
	sliceDeliverySkippedChats  = 2
	sliceDeliveryUpsertFailed  = "daemon.conversation_semantic_sync.upsert_failed"
	sliceDeliveryCheckpointErr = "recording the checkpoint failed"
	sliceDeliveryBusyErr       = "conflicting active job"
	sliceDeliveryMixedChats    = 2
	sliceDeliveryFailureWait   = time.Minute
)

type sliceDeliveryFailureRecord struct {
	Conversations int    `json:"conversations"`
	Err           string `json:"err"`
}

type sliceDeliveryPassRecord struct {
	Message              string   `json:"msg"`
	PartialConversations int      `json:"partial_conversations"`
	FirstListed          []string `json:"first_listed_conversation_ids"`
	Sent                 []string `json:"sent_conversation_ids"`
}

func sliceDeliveryToken(session string, position int) string {
	return fmt.Sprintf("zq%stoken%05dx", strings.ReplaceAll(session, "-", ""), position)
}

func sliceDeliveryID(session string) string {
	return localProofProviderClaude + ":" + session
}

func installSliceDeliveryChat(t *testing.T, home string, session string, messages int, age time.Duration) {
	t.Helper()
	modified := time.Now().Add(-age)
	var body strings.Builder
	for position := range messages {
		digest := sha256.Sum256(fmt.Appendf(nil, "%s-%d", session, position))
		encoded := hex.EncodeToString(digest[:])
		messageUUID := encoded[:8] + "-" + encoded[8:12] + "-4" + encoded[13:16] + "-8" + encoded[17:20] + "-" + encoded[20:32]
		text := "slice delivery record " + sliceDeliveryToken(session, position)
		fmt.Fprintf(&body, `{"type":"user","message":{"role":"user","content":%q},"sessionId":%q,"cwd":%q,"uuid":%q,"timestamp":%q}`+"\n",
			text, session, sliceDeliveryWorkspace, messageUUID, modified.Add(time.Duration(position)*time.Second).Format(time.RFC3339))
	}
	folder := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(sliceDeliveryWorkspace, "/", "-"))
	if err := os.MkdirAll(folder, sliceDeliveryFolderMode); err != nil {
		t.Fatalf("create the transcript folder: %v", err)
	}
	temporary := filepath.Join(home, session+".tmp")
	if err := os.WriteFile(temporary, []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write the transcript: %v", err)
	}
	if err := os.Chtimes(temporary, modified, modified); err != nil {
		t.Fatalf("set the transcript time: %v", err)
	}
	if err := os.Rename(temporary, filepath.Join(folder, session+".jsonl")); err != nil {
		t.Fatalf("install the transcript: %v", err)
	}
}

func sliceDeliveryCheckpointed(t *testing.T, daemon *localProofDaemon, session string) bool {
	t.Helper()
	digest := sha256.Sum256([]byte(sliceDeliveryID(session)))
	name := hex.EncodeToString(digest[:])[:sliceDeliveryNameChars] + ".json"
	matches, err := filepath.Glob(filepath.Join(daemon.roots.State, "clyde", "conversation-semantic", "checkpoints", "*", name))
	if err != nil {
		t.Fatalf("find the checkpoint: %v", err)
	}
	return len(matches) > 0
}

func sliceDeliveryPassRecords(t *testing.T, home string, daemon *localProofDaemon) []sliceDeliveryPassRecord {
	t.Helper()
	records := make([]sliceDeliveryPassRecord, 0)
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
				if !strings.HasSuffix(line, "\n") || !strings.Contains(line, sliceDeliveryPassEvent) {
					continue
				}
				var record sliceDeliveryPassRecord
				if decodeErr := json.Unmarshal([]byte(line), &record); decodeErr != nil {
					return decodeErr
				}
				if record.Message == sliceDeliveryPassEvent {
					records = append(records, record)
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("read the daemon logs: %v", walkErr)
		}
	}
	return records
}

func sliceDeliveryPartialPassCount(t *testing.T, home string, daemon *localProofDaemon) int {
	t.Helper()
	count := 0
	for _, record := range sliceDeliveryPassRecords(t, home, daemon) {
		if record.PartialConversations > 0 {
			count++
		}
	}
	return count
}

func sliceDeliverySawFirstListing(t *testing.T, home string, daemon *localProofDaemon, conversationID string) bool {
	t.Helper()
	return slices.ContainsFunc(sliceDeliveryPassRecords(t, home, daemon), func(record sliceDeliveryPassRecord) bool {
		return slices.Contains(record.FirstListed, conversationID)
	})
}

func sliceDeliveryClaimFolder(t *testing.T, daemon *localProofDaemon) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(daemon.roots.State, "clyde", "conversation-semantic", "checkpoints", ".in-progress", "*"))
	if err != nil {
		t.Fatalf("find the conversation claim folder: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("The test found conversation claim folders %v; it expected one.", matches)
	}
	return matches[0]
}

// holdSliceDeliveryClaim acquires the kernel lock that the daemon uses before writing
// a conversation. The daemon reports the conversation as busy until the test closes
// the file.
func holdSliceDeliveryClaim(t *testing.T, daemon *localProofDaemon, folder string, session string) *os.File {
	t.Helper()
	digest := sha256.Sum256([]byte(sliceDeliveryID(session)))
	path := filepath.Join(folder, hex.EncodeToString(digest[:])[:sliceDeliveryNameChars]+".lock")
	var held *os.File
	// The daemon releases claims between writes. Frequent polling attempts to acquire
	// the claim during that interval, before the large chat finishes.
	pollSliceDelivery(t, daemon, "the conversation claim of "+session, sliceDeliveryClaimDelay, func() bool {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("open the conversation claim: %v", err)
		}
		lockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return false
		}
		if lockErr != nil {
			t.Fatalf("lock the conversation claim: %v", lockErr)
		}
		// The daemon removes the claim file when it finishes. Verify that the lock applies
		// to the current file before using it.
		opened, openedErr := file.Stat()
		current, currentErr := os.Stat(path)
		if openedErr != nil || currentErr != nil || !os.SameFile(opened, current) {
			_ = file.Close()
			return false
		}
		held = file
		return true
	})
	return held
}

func waitForSliceDelivery(t *testing.T, daemon *localProofDaemon, description string, check func() bool) {
	t.Helper()
	pollSliceDelivery(t, daemon, description, sliceDeliveryPollDelay, check)
}

func pollSliceDelivery(t *testing.T, daemon *localProofDaemon, description string, delay time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(sliceDeliveryWaitLimit)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("the test did not observe %s before the limit: %s", description, daemon.stderr.String())
		}
		time.Sleep(delay)
	}
}

// TestLocalFeederReportsOnlyProcessedChatsAsSent delivers two chats in one batch.
// The first chat uses the row limit. The worker does not process the second chat
// in that pass.
func TestLocalFeederReportsOnlyProcessedChatsAsSent(t *testing.T) {
	home := t.TempDir()
	installSliceDeliveryChat(t, home, sliceDeliveryLargeSession, sliceDeliveryLargeMessages, sliceDeliveryLargeAge)
	installSliceDeliveryChat(t, home, sliceDeliveryNewerSession, sliceDeliveryNewerMessages, sliceDeliveryNewerAge)
	settingsPath := filepath.Join(t.TempDir(), "semantic-settings.toml")
	writeLocalProofFile(t, settingsPath, sandboxFastIntervals+sliceDeliverySettings)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	daemon := startLocalProofDaemon(t, buildClydeCLI(t), home, "--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)

	waitForSliceDelivery(t, daemon, "a partial pass", func() bool {
		return sliceDeliveryPartialPassCount(t, home, daemon) > 0
	})
	records := sliceDeliveryPassRecords(t, home, daemon)
	position := slices.IndexFunc(records, func(record sliceDeliveryPassRecord) bool { return record.PartialConversations > 0 })
	first := records[position]
	if first.PartialConversations != sliceDeliverySkippedChats {
		t.Fatalf("The first partial pass reports %d partial conversations; the test expected %d.", first.PartialConversations, sliceDeliverySkippedChats)
	}
	processed := []string{sliceDeliveryID(sliceDeliveryLargeSession)}
	if !slices.Equal(first.Sent, processed) {
		t.Fatalf("The first partial pass reports sent conversations %v; the test expected %v.", first.Sent, processed)
	}
}

// requireMixedBatchFailure waits for an upsert_failed record from a batch with both
// chats. The record must report the checkpoint failure without including the claim error.
func requireMixedBatchFailure(t *testing.T, fixture *memoBatchFixture) {
	t.Helper()
	blockedID := localProofProviderClaude + ":" + memoBatchBlockedSession
	deadline := time.Now().Add(sliceDeliveryFailureWait)
	for {
		for _, line := range daemonLogLines(t, fixture.home, fixture.daemon, sliceDeliveryUpsertFailed) {
			var record sliceDeliveryFailureRecord
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("The test could not decode an upsert_failed record: %v.", err)
			}
			if record.Conversations != sliceDeliveryMixedChats {
				continue
			}
			if !strings.Contains(record.Err, sliceDeliveryCheckpointErr) || !strings.Contains(record.Err, blockedID) {
				t.Fatalf("The upsert_failed record does not include the checkpoint failure of %s: %s.", blockedID, record.Err)
			}
			if strings.Contains(record.Err, sliceDeliveryBusyErr) {
				t.Fatalf("The upsert_failed record includes the claim error: %s.", record.Err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("The daemon did not log a failed batch of %d chats within %v.", sliceDeliveryMixedChats, sliceDeliveryFailureWait)
		}
		time.Sleep(sliceDeliveryPollDelay)
	}
}

// TestLocalFeederReportsCheckpointFailureBesideBusyChat changes two chats. Another
// process claims one chat, and a directory blocks the other chat's checkpoint.
// The test requires a checkpoint failure in the daemon log. Both chats finish after
// the test releases the claim and removes the directory.
func TestLocalFeederReportsCheckpointFailureBesideBusyChat(t *testing.T) {
	fixture := &memoBatchFixture{t: t, home: t.TempDir(), daemon: nil}
	fixture.install(memoBatchChangedSession, memoBatchContentA, memoBatchFirstAge)
	fixture.install(memoBatchBlockedSession, memoBatchOtherContent, memoBatchFirstAge)
	settingsPath := filepath.Join(t.TempDir(), "semantic-settings.toml")
	writeLocalProofFile(t, settingsPath, sandboxFastIntervals)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	fixture.daemon = startLocalProofDaemon(t, buildClydeCLI(t), fixture.home, "--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	fixture.waitFor("the first delivery", func() bool {
		return fixture.checkpoint(memoBatchChangedSession) != "" && fixture.checkpoint(memoBatchBlockedSession) != "" && fixture.noChatsNeeded()
	})
	busyCheckpoint := fixture.checkpoint(memoBatchChangedSession)

	claim := holdSliceDeliveryClaim(t, fixture.daemon, sliceDeliveryClaimFolder(t, fixture.daemon), memoBatchChangedSession)
	blockerPath := fixture.checkpointPath(memoBatchBlockedSession)
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("The test could not remove the checkpoint file: %v.", err)
	}
	if err := os.MkdirAll(filepath.Join(blockerPath, "blocker"), memoBatchFolderMode); err != nil {
		t.Fatalf("The test could not create the directory that blocks the checkpoint: %v.", err)
	}
	fixture.install(memoBatchChangedSession, memoBatchContentB, memoBatchSecondAge)
	fixture.install(memoBatchBlockedSession, memoBatchOtherChanged, memoBatchSecondAge)

	requireMixedBatchFailure(t, fixture)
	if current := fixture.checkpoint(memoBatchChangedSession); current != busyCheckpoint {
		t.Fatalf("The chat with the active claim has a new checkpoint: %s.", current)
	}

	if err := claim.Close(); err != nil {
		t.Fatalf("The test could not release the conversation claim: %v.", err)
	}
	if err := os.RemoveAll(blockerPath); err != nil {
		t.Fatalf("The test could not remove the directory that blocks the checkpoint: %v.", err)
	}
	fixture.waitFor("the checkpoints of both changed chats", func() bool {
		return fixture.checkpoint(memoBatchChangedSession) != busyCheckpoint && fixture.checkpoint(memoBatchBlockedSession) != "" && fixture.noChatsNeeded()
	})
}

// TestLocalFeederServesNewerChatDuringLargeWrite installs a chat after the daemon
// stores a large chat's first slice. The new chat finishes before the daemon records
// the large chat's checkpoint. After both finish, exact searches return both chats.
func TestLocalFeederServesNewerChatDuringLargeWrite(t *testing.T) {
	home := t.TempDir()
	installSliceDeliveryChat(t, home, sliceDeliveryLargeSession, sliceDeliveryLargeMessages, sliceDeliveryLargeAge)
	settingsPath := filepath.Join(t.TempDir(), "semantic-settings.toml")
	writeLocalProofFile(t, settingsPath, sandboxFastIntervals+sliceDeliverySettings)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	daemon := startLocalProofDaemon(t, buildClydeCLI(t), home, "--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)

	waitForSliceDelivery(t, daemon, "a partial pass", func() bool {
		// Read the checkpoint before the partial-pass count. If the checkpoint exists but
		// the later log read does not include a partial pass, the large chat finished without
		// reporting one.
		largeFinished := sliceDeliveryCheckpointed(t, daemon, sliceDeliveryLargeSession)
		if sliceDeliveryPartialPassCount(t, home, daemon) > 0 {
			return true
		}
		if largeFinished {
			t.Fatalf("the large chat has a checkpoint before any partial pass")
		}
		return false
	})
	claimFolder := sliceDeliveryClaimFolder(t, daemon)
	largeClaim := holdSliceDeliveryClaim(t, daemon, claimFolder, sliceDeliveryLargeSession)
	newerClaim := holdSliceDeliveryClaim(t, daemon, claimFolder, sliceDeliveryNewerSession)
	if sliceDeliveryCheckpointed(t, daemon, sliceDeliveryLargeSession) {
		t.Fatalf("the large chat has a checkpoint before the daemon lists the newer chat")
	}
	installSliceDeliveryChat(t, home, sliceDeliveryNewerSession, sliceDeliveryNewerMessages, sliceDeliveryNewerAge)
	newerID := sliceDeliveryID(sliceDeliveryNewerSession)
	waitForSliceDelivery(t, daemon, "the first listing of the newer chat", func() bool {
		return sliceDeliverySawFirstListing(t, home, daemon, newerID)
	})
	// Allow one more large-chat slice before releasing the newer chat's claim. The worker
	// resumes the large chat on rotated turns. It must finish the newer chat before
	// completing the large chat.
	partialPasses := sliceDeliveryPartialPassCount(t, home, daemon)
	if err := largeClaim.Close(); err != nil {
		t.Fatalf("release the large chat claim: %v", err)
	}
	waitForSliceDelivery(t, daemon, "a partial pass after the claim release", func() bool {
		return sliceDeliveryPartialPassCount(t, home, daemon) > partialPasses
	})
	if err := newerClaim.Close(); err != nil {
		t.Fatalf("release the newer chat claim: %v", err)
	}

	largeFinishedFirst := false
	waitForSliceDelivery(t, daemon, "the newer chat checkpoint", func() bool {
		largeFinished := sliceDeliveryCheckpointed(t, daemon, sliceDeliveryLargeSession)
		if sliceDeliveryCheckpointed(t, daemon, sliceDeliveryNewerSession) {
			largeFinishedFirst = largeFinished
			return true
		}
		return false
	})
	if largeFinishedFirst {
		t.Fatalf("the large chat finished before the newer chat")
	}
	waitForSliceDelivery(t, daemon, "the large chat checkpoint", func() bool { return sliceDeliveryCheckpointed(t, daemon, sliceDeliveryLargeSession) })

	checks := []struct {
		session  string
		position int
	}{
		{session: sliceDeliveryNewerSession, position: sliceDeliveryNewerMessages - 1},
		{session: sliceDeliveryLargeSession, position: sliceDeliveryLargeMessages - 1},
	}
	for _, check := range checks {
		conversationID := sliceDeliveryID(check.session)
		for _, surface := range daemon.surfaces() {
			result := mustLocalProofSearch(t, surface, localProofSearchRequest{
				Query: sliceDeliveryToken(check.session, check.position), Limit: sliceDeliverySearchLimit, ConversationID: conversationID,
			})
			found := slices.ContainsFunc(result.Matches, func(match localProofMatch) bool {
				return match.Conversation.ID == conversationID && match.MessageIndex == check.position
			})
			if !found {
				t.Fatalf("%s did not return conversation %s message %d: %+v", surface.name, conversationID, check.position, result.Matches)
			}
		}
	}
}
