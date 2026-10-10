package daemon_test

import (
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
)

func ingestionProofSettled(conversations int) func(conversation.SearchFreshness) bool {
	return func(freshness conversation.SearchFreshness) bool {
		return freshness.LastSyncUnix > 0 && freshness.Needed == 0 && freshness.Pending == 0 &&
			freshness.Manifest == conversations && freshness.Embedded == conversations
	}
}

func TestConversationIngestionProof(t *testing.T) {
	proof := startIngestionProofDaemon(t)

	var afterFirstVersion conversation.SearchFreshness
	t.Run("a_first_version_is_searchable", func(t *testing.T) {
		waitForIngestionProofMatch(t, "a", ingestionProofFirstText, ingestionProofMainSession, ingestionProofFirstToken)
		afterFirstVersion = waitForIngestionProofFreshness(t, "a", ingestionProofSettleTimeout, ingestionProofSettled(1))
	})

	var afterFirstStatus *daemon.RuntimeStatus
	t.Run("a_status_reports_first_ingestion", func(t *testing.T) {
		afterFirstStatus = runStatusProofFirstIngestionStep(t)
	})

	t.Run("b_unchanged_input_needs_no_work", func(t *testing.T) {
		later := waitForIngestionProofFreshness(t, "b", ingestionProofPassTimeout, func(freshness conversation.SearchFreshness) bool {
			return freshness.LastSyncUnix > afterFirstVersion.LastSyncUnix
		})
		if !ingestionProofSettled(1)(later) {
			t.Errorf("step=b freshness=%+v want_needed=0 want_pending=0 want_manifest=1 want_embedded=1", later)
		}
	})

	t.Run("b_status_keeps_totals_after_unchanged_pass", func(t *testing.T) {
		runStatusProofUnchangedPassStep(t, afterFirstStatus)
	})

	t.Run("b_status_command_renders_text_and_json", func(t *testing.T) {
		runStatusProofCommandStep(t)
	})

	t.Run("c_appended_message_is_searchable", func(t *testing.T) {
		proof.install(t, ingestionProofMainSession, ingestionProofMainNumber, []ingestionProofMessage{
			{role: "user", body: ingestionProofFirstText},
			{role: "assistant", body: ingestionProofReplyText},
			{role: "user", body: ingestionProofSecondText},
		}, ingestionProofSecondAge)
		waitForIngestionProofMatch(t, "c", ingestionProofSecondText, ingestionProofMainSession, ingestionProofSecondToken)
		assertIngestionProofMatch(t, "c", ingestionProofFirstText, ingestionProofMainSession, ingestionProofFirstToken, true)
		waitForIngestionProofFreshness(t, "c", ingestionProofSettleTimeout, ingestionProofSettled(1))
	})

	t.Run("d_edited_message_is_searchable", func(t *testing.T) {
		runIngestionProofEditedMessageStep(t, proof)
	})

	t.Run("e_unloadable_transcript_is_reported", func(t *testing.T) {
		runIngestionProofUnloadableTranscriptStep(t, proof)
	})
}
