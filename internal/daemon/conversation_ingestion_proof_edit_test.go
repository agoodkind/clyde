package daemon_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"goodkind.io/clyde/internal/conversation"
)

func runIngestionProofEditedMessageStep(t *testing.T, proof ingestionProofDaemon) {
	t.Helper()
	proof.install(t, ingestionProofMainSession, ingestionProofMainNumber, []ingestionProofMessage{
		{role: "user", body: ingestionProofThirdText},
		{role: "assistant", body: ingestionProofReplyText},
		{role: "user", body: ingestionProofSecondText},
	}, ingestionProofThirdAge)
	proof.install(t, ingestionProofSentinelSession, ingestionProofSentinelNumber, []ingestionProofMessage{
		{role: "user", body: ingestionProofSentinelText},
	}, ingestionProofThirdAge)
	waitForIngestionProofMatch(t, "d", ingestionProofSentinelText, ingestionProofSentinelSession, ingestionProofSentinelToken)
	assertIngestionProofMatch(t, "d", ingestionProofThirdText, ingestionProofMainSession, ingestionProofThirdToken, true)
	assertIngestionProofMatch(t, "d", ingestionProofFirstText, ingestionProofMainSession, ingestionProofFirstToken, ingestionProofReplacedTextStaysStored)
	assertIngestionProofMatch(t, "d", ingestionProofSecondText, ingestionProofMainSession, ingestionProofSecondToken, true)
}

func (proof ingestionProofDaemon) installUnloadable(t *testing.T, step string) {
	t.Helper()
	target := filepath.Join(proof.stagingDir, "unreadable-target.jsonl")
	body := ingestionProofTranscript(ingestionProofUnreadableSession, ingestionProofUnreadableNumber, []ingestionProofMessage{
		{role: "user", body: ingestionProofUnreadableText},
	})
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatalf("step=%s operation=write_transcript path=%s err=%v", step, target, err)
	}
	link := filepath.Join(proof.projectDir, ingestionProofUnreadableSession+".jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("step=%s operation=link_transcript path=%s err=%v", step, link, err)
	}
	modified := unix.NsecToTimespec(time.Now().Add(ingestionProofUnreadableDeferral - ingestionProofGrowthWindow).UnixNano())
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, link, []unix.Timespec{modified, modified}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatalf("step=%s operation=backdate_link path=%s err=%v", step, link, err)
	}
	waitForIngestionProofListing(t, step, ingestionProofUnreadableSession)
	if err := os.Remove(target); err != nil {
		t.Fatalf("step=%s operation=remove_transcript path=%s err=%v", step, target, err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("step=%s operation=replace_transcript_with_directory path=%s err=%v", step, target, err)
	}
}

func runIngestionProofUnloadableTranscriptStep(t *testing.T, proof ingestionProofDaemon) {
	t.Helper()
	proof.installUnloadable(t, "e")
	runStatusProofFailedLoadStep(t)
	waitForIngestionProofFreshness(t, "e", ingestionProofFailureTimeout, func(freshness conversation.SearchFreshness) bool {
		return freshness.Needed == 0 && freshness.Pending == 1 && freshness.Manifest == freshness.Embedded+1
	})
	assertIngestionProofMatch(t, "e", ingestionProofUnreadableText, ingestionProofUnreadableSession, ingestionProofUnreadableToken, false)
}
