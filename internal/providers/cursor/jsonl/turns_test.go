package cursorjsonl

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// collectTurns reads a fixture transcript through the public streaming entry
// point and returns the reconstructed turns.
func collectTurns(t *testing.T, lines []string) []TranscriptMessage {
	t.Helper()

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, path, strings.Join(lines, "\n")+"\n")

	turns := make([]TranscriptMessage, 0)
	err := StreamMessages(path, func(turn TranscriptMessage) error {
		turns = append(turns, turn)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessages returned error: %v", err)
	}
	return turns
}

func turnTexts(turn TranscriptMessage) []string {
	texts := make([]string, 0, len(turn.Parts))
	for _, part := range turn.Parts {
		if part.Type == PartTypeText {
			texts = append(texts, part.Text)
		}
	}
	return texts
}

// TestStreamMessagesGroupsConsecutiveSameRoleRecords proves the primary signal:
// with no boundary marker in the file, a run of consecutive same-role records is
// one turn.
func TestStreamMessagesGroupsConsecutiveSameRoleRecords(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"ask"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"one"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"two"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"three"}]}}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2: %#v", len(turns), turns)
	}
	if turns[1].Role != RoleAssistant {
		t.Fatalf("second turn role = %q, want assistant", turns[1].Role)
	}
	got := turnTexts(turns[1])
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("assistant turn texts = %#v, want [one two three] in file order", got)
	}
}

// TestStreamMessagesSplitsOnTurnEndedBetweenSameRoleRecords covers the boundary
// Cursor states outright. A turn_ended event between two assistant records is a
// real turn boundary that the same-role run alone would miss, so the two runs
// must stay separate turns.
func TestStreamMessagesSplitsOnTurnEndedBetweenSameRoleRecords(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"first turn a"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"first turn b"}]}}`,
		`{"type":"turn_ended","status":"success"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"second turn a"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"second turn b"}]}}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (turn_ended closes the first): %#v", len(turns), turns)
	}
	firstTexts := turnTexts(turns[0])
	secondTexts := turnTexts(turns[1])
	if len(firstTexts) != 2 || firstTexts[0] != "first turn a" || firstTexts[1] != "first turn b" {
		t.Fatalf("first turn texts = %#v, want both first-turn records", firstTexts)
	}
	if len(secondTexts) != 2 || secondTexts[0] != "second turn a" || secondTexts[1] != "second turn b" {
		t.Fatalf("second turn texts = %#v, want both second-turn records", secondTexts)
	}
}

// TestStreamMessagesSplitsOnErrorEvent covers the other typed boundary. A
// standalone error event ends the turn the same way, and it carries an error
// string with no status.
func TestStreamMessagesSplitsOnErrorEvent(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"before a"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"before b"}]}}`,
		`{"type":"error","error":"upstream refused"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"after a"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"after b"}]}}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (the error event closes the first): %#v", len(turns), turns)
	}
	firstTexts := turnTexts(turns[0])
	if len(firstTexts) != 2 || firstTexts[0] != "before a" || firstTexts[1] != "before b" {
		t.Fatalf("first turn texts = %#v, want both records before the error", firstTexts)
	}
	// The outcome is what separates an error event clyde read from a line it
	// could not place: both close the turn, and only the outcome says which.
	if turns[0].Outcome.Uncertain {
		t.Fatalf("first turn outcome = %#v, want certain; clyde read the error event", turns[0].Outcome)
	}
	// Cursor writes the standalone error event with its reason and no status at
	// all, so a reader that allowlists failure statuses reads it as a clean finish
	// and drops the reason. Success is the allowlist instead.
	if !turns[0].Outcome.Failed() {
		t.Fatalf("first turn outcome = %#v, want Failed; the event named a failure", turns[0].Outcome)
	}
	if turns[0].Outcome.Error != "upstream refused" {
		t.Fatalf("first turn error = %q, want the recorded reason", turns[0].Outcome.Error)
	}
}

// TestStreamMessagesEmitsNoEmptyTurnForRepeatedEvents guards against a boundary
// event manufacturing a turn out of nothing. The corpus has runs of consecutive
// turn_ended events, so an event with no open turn must yield nothing.
func TestStreamMessagesEmitsNoEmptyTurnForRepeatedEvents(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"type":"turn_ended","status":"success"}`,
		`{"type":"turn_ended","status":"error","error":"failed"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"the only turn a"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"the only turn b"}]}}`,
		`{"type":"turn_ended","status":"aborted","error":"stopped"}`,
		`{"type":"turn_ended","status":"success"}`,
	})

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1; boundary events must not emit empty turns: %#v", len(turns), turns)
	}
	if turns[0].Role != RoleAssistant {
		t.Fatalf("turn role = %q, want assistant", turns[0].Role)
	}
	if len(turnTexts(turns[0])) != 2 {
		t.Fatalf("turn texts = %#v, want both records of the single turn", turnTexts(turns[0]))
	}
}

// TestStreamMessagesEmitsFinalTurnAtEndOfFile proves the last turn is not lost.
// A turn closes when the next record arrives, so the end of the file has to
// close the open turn too.
func TestStreamMessagesEmitsFinalTurnAtEndOfFile(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"ask"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"tail one"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"tail two"}]}}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2: %#v", len(turns), turns)
	}
	got := turnTexts(turns[1])
	if len(got) != 2 || got[1] != "tail two" {
		t.Fatalf("final turn texts = %#v, want both tail records", got)
	}
}

// TestStreamMessagesInterleavesTextAndToolPartsInFileOrder pins the ordering
// contract at the file-format boundary: the parts of a reconstructed turn stay
// in the order Cursor wrote them, whatever their type.
func TestStreamMessagesInterleavesTextAndToolPartsInFileOrder(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"a"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"first_tool","input":{}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"b"},{"type":"tool_use","name":"second_tool","input":{}}]}}`,
	})

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1: %#v", len(turns), turns)
	}
	parts := turns[0].Parts
	if len(parts) != 4 {
		t.Fatalf("parts = %d, want 4: %#v", len(parts), parts)
	}
	wantTypes := []string{PartTypeText, PartTypeToolUse, PartTypeText, PartTypeToolUse}
	wantTools := []string{"", "first_tool", "", "second_tool"}
	for i, part := range parts {
		if part.Type != wantTypes[i] {
			t.Fatalf("part %d type = %q, want %q; parts must stay in file order", i, part.Type, wantTypes[i])
		}
		if part.ToolName != wantTools[i] {
			t.Fatalf("part %d tool = %q, want %q", i, part.ToolName, wantTools[i])
		}
	}
}

// TestStreamMessagesKeepsRoleBearingLineWithUnknownType guards the fidelity
// risk in reading the type field: the role is what makes a line conversation, so
// a type Cursor adds to conversation records later must not turn the whole
// transcript into unreadable events.
func TestStreamMessagesKeepsRoleBearingLineWithUnknownType(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"type":"message","role":"user","message":{"content":[{"type":"text","text":"still a question"}]}}`,
		`{"type":"message","role":"assistant","message":{"content":[{"type":"text","text":"answer part one"}]}}`,
		`{"type":"message","role":"assistant","message":{"content":[{"type":"text","text":"answer part two"}]}}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2; a typed line carrying a role is conversation and still groups: %#v", len(turns), turns)
	}
	if turns[0].Role != RoleUser || turnTexts(turns[0])[0] != "still a question" {
		t.Fatalf("first turn = %#v", turns[0])
	}
	if got := turnTexts(turns[1]); len(got) != 2 || got[0] != "answer part one" || got[1] != "answer part two" {
		t.Fatalf("assistant turn texts = %#v, want both records joined into one turn", got)
	}
}

// TestStreamMessagesClosesTurnOnUnmodeledEvent covers the reasoning behind
// treating an unrecognized event as a boundary. Every typed role-less line in
// the observed corpus is a turn terminator, and carrying a turn across one would
// assert that no boundary was there. Splitting shows two real fragments, where
// merging invents a turn that never existed, so the reader closes and marks the
// turn uncertain.
func TestStreamMessagesClosesTurnOnUnmodeledEvent(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"part one"}]}}`,
		`{"type":"some_future_event","status":"success"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"part two"}]}}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2; an unmodeled event is a boundary of unknown meaning: %#v", len(turns), turns)
	}
	if !turns[0].Outcome.Uncertain {
		t.Fatalf("first turn outcome = %#v, want Uncertain; the boundary was not readable", turns[0].Outcome)
	}
	if turns[1].Outcome.Uncertain {
		t.Fatalf("second turn outcome = %#v, want certain; it ended at end of file", turns[1].Outcome)
	}
}

// TestStreamMessagesStopsEarlyAfterOneWholeTurn proves a caller can stop after a
// turn without the grouping swallowing the stop signal, and that what it
// received was the whole turn rather than its first record.
func TestStreamMessagesStopsEarlyOnSentinel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"ask one"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"ask two"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"never reached"}]}}`,
	}
	writeTestFile(t, path, strings.Join(lines, "\n")+"\n")

	delivered := make([]TranscriptMessage, 0)
	err := StreamMessages(path, func(turn TranscriptMessage) error {
		delivered = append(delivered, turn)
		return ErrStopStreaming
	})

	if !errors.Is(err, ErrStopStreaming) {
		t.Fatalf("StreamMessages returned %v, want ErrStopStreaming", err)
	}
	if len(delivered) != 1 {
		t.Fatalf("callback ran %d times, want 1; the stop must halt delivery", len(delivered))
	}
	if got := turnTexts(delivered[0]); len(got) != 2 || got[0] != "ask one" || got[1] != "ask two" {
		t.Fatalf("delivered turn texts = %#v, want the whole user turn, not its first record", got)
	}
}
