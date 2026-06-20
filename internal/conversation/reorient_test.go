package conversation_test

import (
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestLatestReorientCheckpointPrefersUsableOverLaterBoundary(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		testBoundaryMessage("boundary-1", transcript.CompactionKindBoundary, nil),
		testSummaryMessage(
			"summary-1",
			"boundary-1",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("recovered context")},
		),
		testBoundaryMessage("boundary-2", transcript.CompactionKindBoundary, nil),
	}

	number, checkpoint := conversation.LatestReorientCheckpoint(conversation.CompactionCheckpoints(messages))
	if checkpoint == nil {
		t.Fatalf("checkpoint = nil, want the usable checkpoint")
	}
	if number != 1 {
		t.Fatalf("checkpoint number = %d, want 1", number)
	}
	if !checkpoint.HasUsableCompactedContext() {
		t.Fatalf("selected checkpoint is not usable: %#v", checkpoint)
	}
	if checkpoint.BoundaryUUID != "boundary-1" {
		t.Fatalf("selected boundary = %q, want boundary-1", checkpoint.BoundaryUUID)
	}
}

func TestLatestReorientCheckpointFallsBackToLatestBoundary(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		testBoundaryMessage("boundary-1", transcript.CompactionKindBoundary, nil),
		testBoundaryMessage("boundary-2", transcript.CompactionKindBoundary, nil),
	}

	number, checkpoint := conversation.LatestReorientCheckpoint(conversation.CompactionCheckpoints(messages))
	if checkpoint == nil {
		t.Fatalf("checkpoint = nil, want the latest boundary")
	}
	if number != 2 {
		t.Fatalf("checkpoint number = %d, want 2", number)
	}
	if checkpoint.HasUsableCompactedContext() {
		t.Fatalf("fallback checkpoint should not be usable: %#v", checkpoint)
	}
	if checkpoint.BoundaryUUID != "boundary-2" {
		t.Fatalf("fallback boundary = %q, want boundary-2", checkpoint.BoundaryUUID)
	}
}

func TestLatestReorientCheckpointEmpty(t *testing.T) {
	t.Parallel()
	number, checkpoint := conversation.LatestReorientCheckpoint(nil)
	if number != 0 || checkpoint != nil {
		t.Fatalf("empty checkpoints = (%d, %#v), want (0, nil)", number, checkpoint)
	}
}
