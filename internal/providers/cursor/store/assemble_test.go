package cursorstore

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bubbleFixture builds one stored bubble without repeating the zero fields at
// every call site.
func bubbleFixture(bubbleID string, bubbleType int, createdAt string, text string) Bubble {
	return Bubble{
		BubbleID:       bubbleID,
		Type:           bubbleType,
		SchemaVersion:  3,
		Text:           text,
		Thinking:       BubbleThinking{Text: ""},
		ToolCall:       nil,
		CreatedAt:      createdAt,
		ServerBubbleID: "",
		RequestID:      "",
	}
}

// withServerID marks a fixture bubble with the server-side identity Cursor keeps
// across a rewrite, which is what makes one row a superseded copy of another.
func withServerID(bubble Bubble, serverBubbleID string) Bubble {
	bubble.ServerBubbleID = serverBubbleID
	return bubble
}

func headerFixture(composerID string, bubbleIDs ...string) ComposerHeader {
	refs := make([]ComposerBubbleRef, 0, len(bubbleIDs))
	for index, bubbleID := range bubbleIDs {
		bubbleType := BubbleTypeAssistant
		if index%2 == 0 {
			bubbleType = BubbleTypeUser
		}
		refs = append(refs, ComposerBubbleRef{BubbleID: bubbleID, Type: bubbleType})
	}
	return ComposerHeader{
		ComposerID:                  composerID,
		Name:                        "",
		CreatedAt:                   0,
		LastUpdatedAt:               0,
		Status:                      "",
		UnifiedMode:                 "",
		ForceMode:                   "",
		LatestChatGenerationUUID:    "",
		FullConversationHeadersOnly: refs,
	}
}

// storedFixture accumulates fixture bubbles in the order Cursor wrote them and
// hands assembly the digests it would have read from the store, so a test can
// state a write order without repeating a rowid at every call site.
type storedFixture struct {
	stored     composerBubbles
	textByID   map[string]string
	writeOrder int64
}

func storedFixtureOf(bubbles ...Bubble) *storedFixture {
	fixture := &storedFixture{
		stored:     emptyComposerBubbles(),
		textByID:   make(map[string]string),
		writeOrder: 0,
	}
	for _, bubble := range bubbles {
		fixture.writeOrder++
		fixture.stored.Digests[bubble.BubbleID] = newBubbleDigest(bubble.BubbleID, bubble, fixture.writeOrder)
		fixture.textByID[bubble.BubbleID] = bubble.Text
	}
	return fixture
}

// withUnreadableRow records that the store holds a row for this bubble that
// Clyde could not parse, which is distinct from the store not holding it at all.
func (fixture *storedFixture) withUnreadableRow(bubbleID string) *storedFixture {
	fixture.stored.Unreadable[bubbleID] = true
	return fixture
}

func (fixture *storedFixture) assembledText(t *testing.T, header ComposerHeader) string {
	t.Helper()

	parts := make([]string, 0)
	for _, bubbleID := range fixture.assembledIDs(t, header) {
		parts = append(parts, fixture.textByID[bubbleID])
	}
	return strings.Join(parts, "|")
}

func (fixture *storedFixture) assembledIDs(t *testing.T, header ComposerHeader) []string {
	t.Helper()

	order, err := assembleBubbleOrder(context.Background(), header.ComposerID, header, fixture.stored)
	if err != nil {
		t.Fatalf("assembleBubbleOrder returned error: %v", err)
	}
	ids := make([]string, 0, len(order))
	for _, entry := range order {
		ids = append(ids, entry.BubbleID)
	}
	return ids
}

func TestAssembleKeepsUnreferencedBubblesThatCarryNewContent(t *testing.T) {
	header := headerFixture("c", "b-user", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-orphan", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "the dropped reply"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the retained reply"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|the dropped reply|the retained reply" {
		t.Fatalf("assembled = %q, want the unreferenced bubble placed by its timestamp", got)
	}
}

func TestAssembleDropsASupersededCopyOfAReferencedBubble(t *testing.T) {
	// Cursor rewrites a turn under a new local bubble id, leaves the old row, and
	// gives both rows the same server bubble id.
	header := headerFixture("c", "b-user", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		withServerID(bubbleFixture("b-superseded", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "the reply"), "s-reply"),
		withServerID(bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"), "s-reply"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|the reply" {
		t.Fatalf("assembled = %q, want the superseded copy dropped", got)
	}
}

func TestAssembleDropsASupersededCopyAmongUnreferencedBubbles(t *testing.T) {
	header := headerFixture("c", "b-user")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		withServerID(bubbleFixture("b-copy1", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "same answer"), "s-answer"),
		withServerID(bubbleFixture("b-copy2", BubbleTypeAssistant, "2026-05-06T05:00:20.000Z", "same answer"), "s-answer"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|same answer" {
		t.Fatalf("assembled = %q, want one copy of the superseded answer", got)
	}
}

// TestAssembleDropsACopyCursorStampedWhenTheKeptRowIsUnstamped is the ordinary
// rewrite shape where only the acknowledged row got a server bubble id. Testing
// the id alone lets the stamped copy skip the content check entirely, because its
// id is unseen, and the same text is emitted twice.
func TestAssembleDropsACopyCursorStampedWhenTheKeptRowIsUnstamped(t *testing.T) {
	header := headerFixture("c", "b-user", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		withServerID(bubbleFixture("b-superseded", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "the reply"), "s-reply"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|the reply" {
		t.Fatalf("assembled = %q, want the stamped copy of an unstamped kept row dropped", got)
	}
}

// TestAssembleDedupesAMixedPairInEitherOrder pins that the answer does not depend
// on which of the two rows Cursor happened to stamp. Both orders describe the
// same pair of stored rows, so both must keep one of them.
func TestAssembleDedupesAMixedPairInEitherOrder(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		stamped string
	}{
		{name: "cursor stamped the earlier row", stamped: "b-first"},
		{name: "cursor stamped the later row", stamped: "b-second"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			first := bubbleFixture("b-first", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "one answer")
			second := bubbleFixture("b-second", BubbleTypeAssistant, "2026-05-06T05:00:20.000Z", "one answer")
			if testCase.stamped == "b-first" {
				first = withServerID(first, "s-answer")
			} else {
				second = withServerID(second, "s-answer")
			}

			header := headerFixture("c", "b-user")
			stored := storedFixtureOf(
				bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
				first,
				second,
			)

			got := stored.assembledText(t, header)
			if got != "the question|one answer" {
				t.Fatalf("assembled = %q, want one copy whichever row carries the server id", got)
			}
		})
	}
}

// TestAssembleKeepsARepeatedTurnCursorGaveItsOwnIdentity is the case a chat-wide
// content key gets wrong. Asking the same question twice, or an agent running the
// identical tool call twice, produces two rows with distinct server bubble ids,
// which is Cursor saying they are separate messages. Measured over 226 real
// chats, 3,358 unreferenced bubbles carrying a distinct server bubble id were
// dropped by the content key, and half of them were tool calls.
func TestAssembleKeepsARepeatedTurnCursorGaveItsOwnIdentity(t *testing.T) {
	call := func() *BubbleToolCall {
		return &BubbleToolCall{Name: "run_terminal", RawArgs: `{"cmd":"make test"}`, Result: "ok", Status: "completed"}
	}
	firstRun := withServerID(bubbleFixture("b-run-1", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", ""), "s-run-1")
	firstRun.ToolCall = call()
	secondRun := withServerID(bubbleFixture("b-run-2", BubbleTypeAssistant, "2026-05-06T05:00:20.000Z", ""), "s-run-2")
	secondRun.ToolCall = call()

	header := headerFixture("c", "b-user")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		firstRun,
		secondRun,
	)

	got := stored.assembledIDs(t, header)
	want := []string{"b-user", "b-run-1", "b-run-2"}
	if !equalIDs(got, want) {
		t.Fatalf("assembled = %v, want both runs of the identical tool call kept: %v", got, want)
	}
}

// TestAssembleKeepsTwoCallsCursorGaveDifferentCallIDs covers the rows Cursor gave
// no server bubble id, where the content fingerprint is the only thing left. Two
// calls the model actually made read identically when the arguments and the
// result happen to match, and the call id is what says they are two.
func TestAssembleKeepsTwoCallsCursorGaveDifferentCallIDs(t *testing.T) {
	lint := func(callID string) *BubbleToolCall {
		return &BubbleToolCall{
			Name:       "read_lints",
			RawArgs:    `{"paths":["main.go"]}`,
			Result:     "no linter errors",
			Status:     "completed",
			ToolCallID: callID,
		}
	}
	firstCall := bubbleFixture("b-lint-1", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "")
	firstCall.ToolCall = lint("call_eZCEfOnkc")
	secondCall := bubbleFixture("b-lint-2", BubbleTypeAssistant, "2026-05-06T05:00:20.000Z", "")
	secondCall.ToolCall = lint("call_PfDLAyT9A")

	header := headerFixture("c", "b-user")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		firstCall,
		secondCall,
	)

	got := stored.assembledIDs(t, header)
	want := []string{"b-user", "b-lint-1", "b-lint-2"}
	if !equalIDs(got, want) {
		t.Fatalf("assembled = %v, want both calls kept because their call ids differ: %v", got, want)
	}
}

// TestAssembleKeepsTwoRunsOfOneCallThatEndedDifferently guards the status. The
// transcript marks a call as errored from its status, so a cancelled run and a
// completed run of the same call render differently, and collapsing them shows a
// cancelled call as successful.
func TestAssembleKeepsTwoRunsOfOneCallThatEndedDifferently(t *testing.T) {
	run := func(status string) *BubbleToolCall {
		return &BubbleToolCall{
			Name:       "run_terminal",
			RawArgs:    `{"cmd":"make test"}`,
			Result:     "",
			Status:     status,
			ToolCallID: "call_eZCEfOnkc",
		}
	}
	cancelled := bubbleFixture("b-cancelled", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "")
	cancelled.ToolCall = run("cancelled")
	completed := bubbleFixture("b-completed", BubbleTypeAssistant, "2026-05-06T05:00:20.000Z", "")
	completed.ToolCall = run("completed")

	header := headerFixture("c", "b-user")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		cancelled,
		completed,
	)

	got := stored.assembledIDs(t, header)
	want := []string{"b-user", "b-cancelled", "b-completed"}
	if !equalIDs(got, want) {
		t.Fatalf("assembled = %v, want the cancelled and completed runs kept apart: %v", got, want)
	}
}

// TestAssembleKeepsAUserBubbleRepeatingAssistantText guards the other half of the
// same defect: a key that ignores the bubble's role collapses a user message into
// an assistant message carrying the same text.
func TestAssembleKeepsAUserBubbleRepeatingAssistantText(t *testing.T) {
	header := headerFixture("c", "b-assistant")
	stored := storedFixtureOf(
		bubbleFixture("b-assistant", BubbleTypeAssistant, "2026-05-06T05:00:00.000Z", "run make check"),
		bubbleFixture("b-user-echo", BubbleTypeUser, "2026-05-06T05:00:10.000Z", "run make check"),
	)

	got := stored.assembledIDs(t, header)
	want := []string{"b-assistant", "b-user-echo"}
	if !equalIDs(got, want) {
		t.Fatalf("assembled = %v, want the user bubble kept beside the assistant one: %v", got, want)
	}
}

// TestAssembleKeepsBothRowsWhenCursorRecordedNoIdentity covers the rows Cursor
// never gave a server bubble id. Identical content is not evidence of a copy when
// nothing recorded an identity either way: it is equally the shape of a turn that
// happened twice, and the store does not separate the two.
//
// This replaces a test that required the opposite. That one asserted the second
// row was dropped, which pinned the data loss rather than the behaviour: a
// duplicate a reader can see and ignore is recoverable, and a message assembly
// deleted is not.
func TestAssembleKeepsBothRowsWhenCursorRecordedNoIdentity(t *testing.T) {
	header := headerFixture("c", "b-user", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-repeat", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "the reply"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
	)

	got := stored.assembledIDs(t, header)
	want := []string{"b-user", "b-repeat", "b-reply"}
	if !equalIDs(got, want) {
		t.Fatalf("assembled = %v, want both rows kept because nothing says either is a copy: %v", got, want)
	}
}

// TestAssembleStillDropsACopyCursorStampedOnOneSide is the other half, so
// keeping the identity-less pair does not disable supersession itself. Exactly
// one row carrying a server id is Cursor saying which one it acknowledged.
func TestAssembleStillDropsACopyCursorStampedOnOneSide(t *testing.T) {
	header := headerFixture("c", "b-user", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		withServerID(bubbleFixture("b-superseded", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "the reply"), "s-reply"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|the reply" {
		t.Fatalf("assembled = %q, want the copy Cursor stamped dropped", got)
	}
}

// TestFingerprintKeepsLeadingWhitespace covers an indented code block. Trimming
// the content collapses a message holding one into the same message unindented,
// and the indentation is what makes it a code block.
func TestFingerprintKeepsLeadingWhitespace(t *testing.T) {
	plain := bubbleFixture("b-plain", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "code")
	indented := bubbleFixture("b-indented", BubbleTypeAssistant, "2026-05-06T05:00:20.000Z", "    code")

	if fingerprintOf(plain) == fingerprintOf(indented) {
		t.Fatal("a message and its indented form share a fingerprint, so one of them is dropped as a copy")
	}

	header := headerFixture("c", "b-user")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		plain,
		indented,
	)
	got := stored.assembledIDs(t, header)
	if !equalIDs(got, []string{"b-user", "b-plain", "b-indented"}) {
		t.Fatalf("assembled = %v, want the indented code block kept beside the plain one", got)
	}
}

func TestAssemblePreservesHeaderOrderWhenTimestampsRunBackwards(t *testing.T) {
	// Cursor's write times are coarse enough that the header order and the
	// timestamps disagree. The header wins for the bubbles it references.
	header := headerFixture("c", "b-1", "b-2", "b-3")
	stored := storedFixtureOf(
		bubbleFixture("b-1", BubbleTypeUser, "2026-05-06T05:00:30.000Z", "first"),
		bubbleFixture("b-2", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "second"),
		bubbleFixture("b-3", BubbleTypeUser, "2026-05-06T05:00:20.000Z", "third"),
	)

	got := stored.assembledText(t, header)
	if got != "first|second|third" {
		t.Fatalf("assembled = %q, want the header order preserved verbatim", got)
	}
}

// TestAssembleEmitsARepeatedHeaderReferenceOnce covers a header that lists the
// same bubble twice. Querying a real global store, 1 of 400 sampled
// `composerData:` rows has more `fullConversationHeadersOnly` entries than
// distinct bubble ids, and following the list literally reads that row twice and
// repeats the turn in the transcript.
func TestAssembleEmitsARepeatedHeaderReferenceOnce(t *testing.T) {
	header := headerFixture("c", "b-user", "b-reply", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
	)

	got := stored.assembledIDs(t, header)
	want := []string{"b-user", "b-reply"}
	if !equalIDs(got, want) {
		t.Fatalf("assembled = %v, want the repeated reference emitted once: %v", got, want)
	}
}

// TestAssemblePlacesAnAdditionPastAHeaderJump is the ordering defect a running
// maximum introduces. The header's second bubble is stamped far ahead of its
// neighbours, so a high-water mark stays there for the rest of the chat and
// emits every later addition at the jump instead of between the two header
// bubbles it actually sits between.
func TestAssemblePlacesAnAdditionPastAHeaderJump(t *testing.T) {
	header := headerFixture("c", "b-1", "b-2", "b-3", "b-4")
	stored := storedFixtureOf(
		bubbleFixture("b-1", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "first"),
		// One header bubble carries a timestamp from far later in the chat.
		bubbleFixture("b-2", BubbleTypeAssistant, "2026-05-06T05:09:00.000Z", "second"),
		bubbleFixture("b-3", BubbleTypeUser, "2026-05-06T05:00:30.000Z", "third"),
		bubbleFixture("b-4", BubbleTypeAssistant, "2026-05-06T05:00:50.000Z", "fourth"),
		// This addition sits between the third and fourth header bubbles.
		bubbleFixture("b-add", BubbleTypeAssistant, "2026-05-06T05:00:40.000Z", "addition"),
	)

	got := stored.assembledText(t, header)
	if got != "first|second|third|addition|fourth" {
		t.Fatalf("assembled = %q, want the addition placed at its own point rather than at the jump", got)
	}
}

// TestAssembleDoesNotWidenAGapThroughAnUndatedHeaderBubble covers a header bubble
// Cursor stored with no `createdAt`. Reading its empty timestamp as the gap's
// lower bound reopens the gap back to the start of the chat, and because the last
// bracketing gap wins, that wide gap then claims an addition that opened the
// conversation and places it next to the chat's last turn.
func TestAssembleDoesNotWidenAGapThroughAnUndatedHeaderBubble(t *testing.T) {
	header := headerFixture("c", "b-1", "b-undated", "b-3")
	stored := storedFixtureOf(
		bubbleFixture("b-opening", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "opening"),
		bubbleFixture("b-1", BubbleTypeUser, "2026-05-06T05:00:10.000Z", "first"),
		bubbleFixture("b-undated", BubbleTypeAssistant, "", "undated header bubble"),
		bubbleFixture("b-3", BubbleTypeUser, "2026-05-06T05:00:30.000Z", "third"),
	)

	got := stored.assembledText(t, header)
	if got != "opening|first|undated header bubble|third" {
		t.Fatalf("assembled = %q, want the addition placed before the header rather than inside it", got)
	}
}

func TestAssembleKeepsAddedBubblesInTimeOrderAmongThemselves(t *testing.T) {
	// Several unreferenced bubbles share one insertion point. They must stay in
	// their own ascending order rather than being reversed.
	header := headerFixture("c", "b-user", "b-late")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-o1", BubbleTypeAssistant, "2026-05-06T05:01:00.000Z", "step one"),
		bubbleFixture("b-o2", BubbleTypeAssistant, "2026-05-06T05:02:00.000Z", "step two"),
		bubbleFixture("b-o3", BubbleTypeAssistant, "2026-05-06T05:03:00.000Z", "step three"),
		bubbleFixture("b-late", BubbleTypeAssistant, "2026-05-06T05:09:00.000Z", "the last reply"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|step one|step two|step three|the last reply" {
		t.Fatalf("assembled = %q, want the added bubbles in ascending time order", got)
	}
}

// TestAssembleOrdersTiedAdditionsByWriteOrder pins the tiebreaker. Two additions
// share a millisecond, which is ordinary rather than rare: one real chat holds
// 861 adjacent header pairs sharing a value. Their bubble ids sort opposite to
// the order Cursor wrote them, so ordering by uuid reverses them.
func TestAssembleOrdersTiedAdditionsByWriteOrder(t *testing.T) {
	header := headerFixture("c", "b-user")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("zzz-written-first", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "written first"),
		bubbleFixture("aaa-written-second", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "written second"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|written first|written second" {
		t.Fatalf("assembled = %q, want the tied additions in the store's write order", got)
	}
}

func TestAssemblePlacesATiedBubbleAfterTheReferencedOne(t *testing.T) {
	header := headerFixture("c", "b-user", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
		bubbleFixture("b-tied", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the tied addition"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|the reply|the tied addition" {
		t.Fatalf("assembled = %q, want the tied bubble after the referenced one", got)
	}
}

func TestAssembleAppendsBubblesWithNoTimestamp(t *testing.T) {
	header := headerFixture("c", "b-user", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-earliest", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "dated addition"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
		bubbleFixture("b-undated", BubbleTypeAssistant, "", "undated addition"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|dated addition|the reply|undated addition" {
		t.Fatalf("assembled = %q, want the undated bubble appended after the header order", got)
	}
}

func TestAssembleSkipsUnreferencedBubblesWithNoContent(t *testing.T) {
	header := headerFixture("c", "b-user")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-empty", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", "   "),
	)

	got := stored.assembledIDs(t, header)
	if !equalIDs(got, []string{"b-user"}) {
		t.Fatalf("assembled = %v, want the empty unreferenced bubble skipped", got)
	}
}

func TestAssembleKeepsAReferencedBubbleWhoseRowIsMissing(t *testing.T) {
	header := headerFixture("c", "b-user", "b-gone", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
	)

	got := stored.assembledText(t, header)
	if got != "the question|the reply" {
		t.Fatalf("assembled = %q, want the absent reference skipped without disturbing the rest", got)
	}
}

func TestStreamComposerBubblesLogsAnAbsentHeaderReference(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-user', '{"_v":3,"type":1,"bubbleId":"b-user","text":"the question"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c', '{"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b-user","type":1},{"bubbleId":"b-gone","type":2}]}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	header, found, err := ReadComposerHeader(context.Background(), readonly, "c")
	if err != nil || !found {
		t.Fatalf("ReadComposerHeader found = %v, err = %v", found, err)
	}

	var logOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	err = StreamComposerBubbles(context.Background(), readonly, "c", header, func(Bubble) bool {
		return true
	})
	if err != nil {
		t.Fatalf("StreamComposerBubbles returned error: %v", err)
	}
	if !strings.Contains(logOutput.String(), "providers.cursor.store.referenced_bubble_absent") {
		t.Fatalf("log = %s, want the unresolved header reference reported", logOutput.String())
	}
}

// TestAssembleFailsOnAReferencedBubbleTheStoreHoldsButCannotParse separates the
// two reasons a referenced bubble has no digest. A row the store does not hold is
// nothing to read; a row the store holds and Clyde cannot parse is a message the
// header ordered into the conversation, and omitting it would present a
// transcript Clyde could not read completely as a complete one.
func TestAssembleFailsOnAReferencedBubbleTheStoreHoldsButCannotParse(t *testing.T) {
	header := headerFixture("c", "b-user", "b-broken", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
	).withUnreadableRow("b-broken")

	_, err := assembleBubbleOrder(context.Background(), header.ComposerID, header, stored.stored)
	if err == nil {
		t.Fatal("assembleBubbleOrder returned no error for a referenced bubble the store holds and cannot parse")
	}
	if !strings.Contains(err.Error(), "b-broken") {
		t.Fatalf("error = %v, want it to name the bubble it could not read", err)
	}
}

// TestAssembleFailsOnAReferenceWhenRowsWentUnaccountedFor covers a reference with
// no digest while the read could not account for every row of the range. Skipping
// it rests on the read having seen everything: a reference with no digest is only
// a row the store does not hold when nothing went unread, and the two cannot be
// told apart once something did.
func TestAssembleFailsOnAReferenceWhenRowsWentUnaccountedFor(t *testing.T) {
	header := headerFixture("c", "b-user", "b-gone", "b-reply")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		bubbleFixture("b-reply", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "the reply"),
	)
	stored.stored.Unaccounted = 1

	_, err := assembleBubbleOrder(context.Background(), header.ComposerID, header, stored.stored)
	if err == nil {
		t.Fatal("assembleBubbleOrder returned no error for a missing reference while a row of the chat went unread")
	}
	if !strings.Contains(err.Error(), "b-gone") {
		t.Fatalf("error = %v, want it to name the reference it could not account for", err)
	}
}

// TestStreamComposerBubblesFailsWhenAnOrderedRowCannotBeParsed covers a row the
// ordering pass read, found content in and placed in the transcript, which the
// per-bubble decode then rejects. The two passes disagreeing is a fault in
// Clyde's own reading rather than a property of the store, so the conversation
// fails rather than being served without a message assembly committed to
// emitting.
//
// This replaces a test that required the opposite. That one asserted the row
// vanished while the stream reported success, which pinned a silent partial
// result. Measured on a real store, no bubble row has a member type the decoder
// rejects, so this failure mode is a disagreement to surface rather than a shape
// Cursor writes.
func TestStreamComposerBubblesFailsWhenAnOrderedRowCannotBeParsed(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-user', '{"_v":3,"type":1,"bubbleId":"b-user","text":"the question","createdAt":"2026-05-06T05:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-reply', '{"_v":3,"type":2,"bubbleId":"b-reply","text":"the reply","createdAt":"2026-05-06T05:00:30.000Z"}')`,
		// Unreferenced, valid JSON so the ordering pass keeps it, and a member type
		// the per-bubble decode rejects.
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-orphan', '{"_v":3,"type":2,"bubbleId":"b-orphan","text":17,"createdAt":"2026-05-06T05:00:10.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c', '{"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b-user","type":1},{"bubbleId":"b-reply","type":2}]}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	header, found, err := ReadComposerHeader(context.Background(), readonly, "c")
	if err != nil || !found {
		t.Fatalf("ReadComposerHeader found = %v, err = %v", found, err)
	}

	err = StreamComposerBubbles(context.Background(), readonly, "c", header, func(bubble Bubble) bool {
		return true
	})
	if err == nil {
		t.Fatal("StreamComposerBubbles returned no error for an ordered row it could not parse")
	}
	if !strings.Contains(err.Error(), "b-orphan") {
		t.Fatalf("error = %v, want it to name the row it could not read", err)
	}
}

// TestStreamComposerBubblesFailsWhenAnOrderedRowIsRewritten covers the window
// between the digest pass and the exact-value pass. The first pass placed the
// row using its old timestamp, so emitting replacement content at that position
// would combine two database revisions into one transcript.
func TestStreamComposerBubblesFailsWhenAnOrderedRowIsRewritten(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-user', '{"_v":3,"type":1,"bubbleId":"b-user","text":"the question","createdAt":"2026-05-06T05:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-reply', '{"_v":3,"type":2,"bubbleId":"b-reply","text":"the old reply","createdAt":"2026-05-06T05:00:30.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c', '{"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b-user","type":1},{"bubbleId":"b-reply","type":2}]}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open writable database returned error: %v", err)
	}
	t.Cleanup(func() { _ = writable.Close() })

	header, found, err := ReadComposerHeader(context.Background(), readonly, "c")
	if err != nil || !found {
		t.Fatalf("ReadComposerHeader found = %v, err = %v", found, err)
	}

	served := make([]string, 0)
	var rewriteErr error
	err = StreamComposerBubbles(context.Background(), readonly, "c", header, func(bubble Bubble) bool {
		served = append(served, bubble.Text)
		if bubble.BubbleID == "b-user" {
			_, rewriteErr = writable.Exec(
				`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-reply', '{"_v":3,"type":2,"bubbleId":"b-reply","text":"the replacement","createdAt":"2026-05-06T04:59:00.000Z"}')`,
			)
		}
		return true
	})
	if rewriteErr != nil {
		t.Fatalf("rewrite ordered row: %v", rewriteErr)
	}
	if err == nil {
		t.Fatalf("StreamComposerBubbles returned no error after an ordered row was rewritten; served %v", served)
	}
	if !strings.Contains(err.Error(), "b-reply") {
		t.Fatalf("error = %v, want it to name the rewritten row", err)
	}
}

// TestStreamComposerBubblesFailsOnANullValuedReferencedRow covers the row shape
// that escapes both passes. SQLite's json_valid returns NULL for a NULL value, so
// a NULL row satisfies neither the projection's predicate nor its negation while
// count(*) still counts it. Cursor writes these: the operator's store holds 409
// NULL-valued rows, 3 of them in the bubble range.
func TestStreamComposerBubblesFailsOnANullValuedReferencedRow(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-user', '{"_v":3,"type":1,"bubbleId":"b-user","text":"the question","createdAt":"2026-05-06T05:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-null', NULL)`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c', '{"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b-user","type":1},{"bubbleId":"b-null","type":2}]}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	header, found, err := ReadComposerHeader(context.Background(), readonly, "c")
	if err != nil || !found {
		t.Fatalf("ReadComposerHeader found = %v, err = %v", found, err)
	}

	err = StreamComposerBubbles(context.Background(), readonly, "c", header, func(bubble Bubble) bool {
		return true
	})
	if err == nil {
		t.Fatal("StreamComposerBubbles returned no error for a header-referenced NULL-valued row")
	}
	if !strings.Contains(err.Error(), "b-null") {
		t.Fatalf("error = %v, want it to name the row it could not read", err)
	}
}

// TestReadComposerBubbleDigestsAccountsForANullValuedRow pins the accounting the
// fatal rule rests on: a NULL row is named as unreadable rather than leaving the
// totals level.
func TestReadComposerBubbleDigestsAccountsForANullValuedRow(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-user', '{"_v":3,"type":1,"bubbleId":"b-user","text":"the question"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-null', NULL)`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-text', 'not json at all')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	stored, err := readComposerBubbleDigests(context.Background(), readonly, "c")
	if err != nil {
		t.Fatalf("readComposerBubbleDigests returned error: %v", err)
	}
	if len(stored.Digests) != 1 {
		t.Fatalf("digests = %d, want the one row that parses", len(stored.Digests))
	}
	if !stored.Unreadable["b-null"] || !stored.Unreadable["b-text"] {
		t.Fatalf("unreadable = %v, want both the NULL row and the non-JSON row named", stored.Unreadable)
	}
	if stored.Unaccounted != 0 {
		t.Fatalf("Unaccounted = %d, want every row accounted for", stored.Unaccounted)
	}
}

// TestDecodeBubbleAcceptsAnObjectValuedToolResult guards the union in Cursor's
// tool result. Some tools write the result as a JSON object rather than a
// string, and a decoder that accepts only the string form fails the whole
// bubble. Measured on a real store, 568 of 536,875 bubble rows carry the object
// form, and in one chat 5 of them were referenced by the composer header, so the
// failure took the conversation with it.
func TestDecodeBubbleAcceptsAnObjectValuedToolResult(t *testing.T) {
	objectResult := []byte(`{"_v":3,"type":2,"bubbleId":"b","text":"",` +
		`"toolFormerData":{"name":"todo_write","rawArgs":"{}","status":"completed",` +
		`"result":{"success":true,"finalTodos":[]}}}`)

	bubble, err := DecodeBubbleJSON(objectResult)
	if err != nil {
		t.Fatalf("DecodeBubbleJSON returned error for an object-valued tool result: %v", err)
	}
	if bubble.ToolCall == nil {
		t.Fatal("tool call missing")
	}
	if !strings.Contains(string(bubble.ToolCall.Result), `"success":true`) {
		t.Fatalf("tool result = %q, want the object preserved as raw JSON", bubble.ToolCall.Result)
	}
	if !bubble.HasContent() {
		t.Fatal("a bubble carrying only a tool call reported no content")
	}
}

func TestDecodeBubbleStillAcceptsAStringToolResult(t *testing.T) {
	stringResult := []byte(`{"_v":3,"type":2,"bubbleId":"b","text":"",` +
		`"toolFormerData":{"name":"read_file","rawArgs":"{}","status":"completed","result":"file body"}}`)

	bubble, err := DecodeBubbleJSON(stringResult)
	if err != nil {
		t.Fatalf("DecodeBubbleJSON returned error: %v", err)
	}
	if string(bubble.ToolCall.Result) != "file body" {
		t.Fatalf("tool result = %q, want the plain string", bubble.ToolCall.Result)
	}
}

func TestAssembleDropsASupersededCopyOfAToolCall(t *testing.T) {
	call := func() *BubbleToolCall {
		return &BubbleToolCall{Name: "read_file", RawArgs: `{"path":"main.go"}`, Result: "body", Status: "completed"}
	}
	superseded := withServerID(bubbleFixture("b-tool-old", BubbleTypeAssistant, "2026-05-06T05:00:10.000Z", ""), "s-tool")
	superseded.ToolCall = call()
	referenced := withServerID(bubbleFixture("b-tool", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", ""), "s-tool")
	referenced.ToolCall = call()

	header := headerFixture("c", "b-user", "b-tool")
	stored := storedFixtureOf(
		bubbleFixture("b-user", BubbleTypeUser, "2026-05-06T05:00:00.000Z", "the question"),
		superseded,
		referenced,
	)

	got := stored.assembledIDs(t, header)
	if !equalIDs(got, []string{"b-user", "b-tool"}) {
		t.Fatalf("assembled = %v, want the superseded tool call dropped", got)
	}
}

// TestStreamComposerBubblesDoesNotReadWholeValuesToOrderAChat is the cost
// contract on the ordering pass. Ordering has to see every stored bubble's write
// time before it can place any of them, so it cannot stop early, but it must not
// pull the parts of a bubble it does not read: a real store's widest chat holds
// 22,724 rows and 933 MB of payload, and a search that inlines a window beside
// each hit pays that per hit.
//
// The assertion is on bytes allocated rather than on the heap at one instant,
// because the values are streamed and released as they go, so a heap reading
// after a collection is the same whether or not every value was pulled through.
func TestStreamComposerBubblesDoesNotReadWholeValuesToOrderAChat(t *testing.T) {
	const laterBubbles = 24
	const unreadBytesPerBubble = 2 << 20
	const chatBytes = int64(laterBubbles) * unreadBytesPerBubble

	dbPath := createLargeComposerTestDatabase(t, laterBubbles, unreadBytesPerBubble)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	header, found, err := ReadComposerHeader(context.Background(), readonly, "composer-big")
	if err != nil || !found {
		t.Fatalf("ReadComposerHeader found = %v, err = %v", found, err)
	}

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	served := 0
	var afterFirst runtime.MemStats
	err = StreamComposerBubbles(context.Background(), readonly, "composer-big", header, func(bubble Bubble) bool {
		served++
		runtime.ReadMemStats(&afterFirst)
		return false
	})
	if err != nil {
		t.Fatalf("StreamComposerBubbles returned error: %v", err)
	}
	if served != 1 {
		t.Fatalf("served = %d bubbles, want the read to stop after the first", served)
	}

	// TotalAlloc is process wide, so anything else allocating in this window counts
	// against the reading. No test in this package runs in parallel, and the budget
	// sits roughly 280 times above what the projection actually allocates, so the
	// margin absorbs background noise while still failing on a revert, which
	// allocates the whole chat.
	allocated := int64(afterFirst.TotalAlloc) - int64(before.TotalAlloc)
	budget := chatBytes / 4
	t.Logf("allocated %d bytes ordering a %d byte chat", allocated, chatBytes)
	if allocated > budget {
		t.Fatalf("allocated %d bytes serving one bubble of a %d byte chat, want under %d",
			allocated, chatBytes, budget)
	}
}

// TestStreamComposerBubblesFailsOnAnUnparsableReferencedRow drives the same rule
// as the assembly-level test through a real database, covering both shapes of
// unparsable row: a value that is not JSON at all, which the ordering pass names,
// and a value that is JSON whose members carry the wrong types, which only the
// per-bubble decode rejects.
func TestStreamComposerBubblesFailsOnAnUnparsableReferencedRow(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
	}{
		{name: "value is not json", value: "this is not json"},
		{name: "value is json with the wrong member types", value: `{"_v":3,"type":2,"bubbleId":"b-broken","text":17}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dbPath := writeCursorTestDatabase(t, []string{
				`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-user', '{"_v":3,"type":1,"bubbleId":"b-user","text":"the question","createdAt":"2026-05-06T05:00:00.000Z"}')`,
				fmt.Sprintf(`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-broken', %s)`, quoteSQLText(testCase.value)),
				`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c', '{"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b-user","type":1},{"bubbleId":"b-broken","type":2}]}')`,
			})
			readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
			if err != nil {
				t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
			}
			t.Cleanup(func() { _ = readonly.Close() })

			header, found, err := ReadComposerHeader(context.Background(), readonly, "c")
			if err != nil || !found {
				t.Fatalf("ReadComposerHeader found = %v, err = %v", found, err)
			}

			err = StreamComposerBubbles(context.Background(), readonly, "c", header, func(bubble Bubble) bool {
				return true
			})
			if err == nil {
				t.Fatal("StreamComposerBubbles returned no error for a header-referenced row it could not parse")
			}
			if !strings.Contains(err.Error(), "b-broken") {
				t.Fatalf("error = %v, want it to name the bubble it could not read", err)
			}
		})
	}
}

// TestReadComposerBubbleStockSeparatesADraftFromAStoredChat is the discovery
// gate's evidence. A chat's header reference list can be empty while its key
// range holds a whole conversation, so the range is what says whether the chat
// has anything to deliver.
func TestReadComposerBubbleStockSeparatesADraftFromAStoredChat(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c-draft', '{"composerId":"c-draft","fullConversationHeadersOnly":[]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c-stored', '{"composerId":"c-stored","fullConversationHeadersOnly":[]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c-stored:b-1', '{"_v":3,"type":1,"bubbleId":"b-1","text":"the question","createdAt":"2026-05-06T05:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c-blank', '{"composerId":"c-blank","fullConversationHeadersOnly":[]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c-blank:b-1', '{"_v":3,"type":1,"bubbleId":"b-1","text":"  "}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	for _, testCase := range []struct {
		composerID  string
		wantRows    int64
		wantContent bool
	}{
		{composerID: "c-draft", wantRows: 0, wantContent: false},
		{composerID: "c-stored", wantRows: 1, wantContent: true},
		{composerID: "c-blank", wantRows: 1, wantContent: false},
	} {
		stock, err := ReadComposerBubbleStock(context.Background(), readonly, testCase.composerID)
		if err != nil {
			t.Fatalf("ReadComposerBubbleStock(%q) returned error: %v", testCase.composerID, err)
		}
		if stock.StoredRows != testCase.wantRows || stock.HasContent != testCase.wantContent {
			t.Fatalf("stock(%q) = %+v, want rows %d and content %v",
				testCase.composerID, stock, testCase.wantRows, testCase.wantContent)
		}
	}
}

func TestReadSnapshotPinsBubbleCountAndProjectionToOneWALRevision(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-1', '{"_v":3,"type":1,"bubbleId":"b-1","text":"first"}')`,
	})
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open writable database returned error: %v", err)
	}
	t.Cleanup(func() { _ = writable.Close() })
	if _, err := writable.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	snapshot, err := beginReadSnapshot(context.Background(), readonly)
	if err != nil {
		t.Fatalf("beginReadSnapshot returned error: %v", err)
	}
	t.Cleanup(snapshot.rollback)
	bounds := keyRangeForPrefix("bubbleId:c:")
	storedRows, err := snapshot.countRange(context.Background(), KVTableCursorDiskKV, bounds)
	if err != nil {
		t.Fatalf("countRange returned error: %v", err)
	}

	if _, err := writable.Exec(
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-2', '{"_v":3,"type":2,"bubbleId":"b-2","text":"second"}')`,
	); err != nil {
		t.Fatalf("insert concurrent bubble: %v", err)
	}

	projected := 0
	err = forEachComposerBubbleProjection(context.Background(), snapshot, bounds, func(bubbleProjection) error {
		projected++
		return nil
	})
	if err != nil {
		t.Fatalf("forEachComposerBubbleProjection returned error: %v", err)
	}
	if storedRows != 1 || projected != storedRows {
		t.Fatalf("snapshot count = %d and projection = %d, want both to see the one-row revision", storedRows, projected)
	}

	freshStock, err := ReadComposerBubbleStock(context.Background(), readonly, "c")
	if err != nil {
		t.Fatalf("ReadComposerBubbleStock returned error: %v", err)
	}
	if freshStock.StoredRows != 2 {
		t.Fatalf("fresh stock rows = %d, want proof the concurrent row committed", freshStock.StoredRows)
	}
}

func TestReadComposerBubbleStockLogsCountFailureBeforeReturningIt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open writable database returned error: %v", err)
	}
	if _, err := writable.Exec("CREATE TABLE cursorDiskKV(value BLOB)"); err != nil {
		_ = writable.Close()
		t.Fatalf("create malformed cursorDiskKV table: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable database: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	var logOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	if _, err := ReadComposerBubbleStock(context.Background(), readonly, "c"); err == nil {
		t.Fatal("ReadComposerBubbleStock returned no error for a table without its key column")
	}
	if !strings.Contains(logOutput.String(), "providers.cursor.store.composer_bubble_count_failed") {
		t.Fatalf("log = %s, want the count failure reported before the wrapped error", logOutput.String())
	}
}

// createLargeComposerTestDatabase writes a chat whose bulk sits in the members
// assembly never reads, which is the shape a real chat has: Cursor stores each
// bubble with its attached codebase context beside the message, and the fields
// ordering needs are a fraction of the row. Measured on a real store's widest
// chat, they are 267 MB of 933 MB.
func createLargeComposerTestDatabase(t *testing.T, laterBubbles int, bubbleBytes int) string {
	t.Helper()

	refs := make([]string, 0, laterBubbles+1)
	statements := make([]string, 0, laterBubbles+2)
	refs = append(refs, `{"bubbleId":"b-000","type":1}`)
	statements = append(statements, fmt.Sprintf(
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-big:b-000', '{"_v":3,"type":1,"bubbleId":"b-000","text":"open","createdAt":"2026-05-06T05:00:00.000Z"}')`,
	))
	for index := range laterBubbles {
		bubbleID := fmt.Sprintf("b-%03d", index+1)
		refs = append(refs, fmt.Sprintf(`{"bubbleId":%q,"type":2}`, bubbleID))
		statements = append(statements, fmt.Sprintf(
			`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-big:%s', `+
				`'{"_v":3,"type":2,"bubbleId":%q,"text":"reply %d","createdAt":"2026-05-06T05:%02d:00.000Z",`+
				`"attachedCodeChunks":[{"content":%q}]}')`,
			bubbleID, bubbleID, index+1, index+1, strings.Repeat("x", bubbleBytes),
		))
	}
	statements = append(statements, fmt.Sprintf(
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:composer-big', '{"composerId":"composer-big","fullConversationHeadersOnly":[%s]}')`,
		strings.Join(refs, ","),
	))
	return writeCursorTestDatabase(t, statements)
}

// quoteSQLText renders one literal for a fixture statement, so a fixture value
// carrying a quote cannot change the statement around it.
func quoteSQLText(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func writeCursorTestDatabase(t *testing.T, statements []string) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	schema := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
	}
	for _, statement := range append(schema, statements...) {
		if _, err := writable.Exec(statement); err != nil {
			_ = writable.Close()
			t.Fatalf("exec failed: %v", err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
	return dbPath
}

func equalIDs(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// TestReadComposerBubbleDigestsCountsAnUnaccountedRowFromARealDatabase is the
// line the whole fatal rule rests on, exercised against a database rather than a
// hand-set field. A row that satisfies neither the projection's predicate nor its
// negation is what an implementation always evaluating this to zero would hide.
func TestReadComposerBubbleDigestsCountsAnUnaccountedRowFromARealDatabase(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-user', '{"_v":3,"type":1,"bubbleId":"b-user","text":"the question"}')`,
		// A key that does not belong to this chat's bubble shape, so the projection
		// reads it and cannot name it, which is a row the range counted and the
		// digests do not hold.
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:', '{"_v":3,"type":2,"text":"unnameable"}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	stored, err := readComposerBubbleDigests(context.Background(), readonly, "c")
	if err != nil {
		t.Fatalf("readComposerBubbleDigests returned error: %v", err)
	}
	if stored.Unaccounted != 1 {
		t.Fatalf("Unaccounted = %d, want the row the read could neither name nor call unreadable", stored.Unaccounted)
	}

	// And a reference with no digest is then fatal rather than skipped.
	header := ComposerHeader{
		ComposerID: "c",
		FullConversationHeadersOnly: []ComposerBubbleRef{
			{BubbleID: "b-user", Type: BubbleTypeUser},
			{BubbleID: "b-missing", Type: BubbleTypeAssistant},
		},
	}
	if _, err := assembleBubbleOrder(context.Background(), "c", header, stored); err == nil {
		t.Fatal("assembleBubbleOrder returned no error for a missing reference while a row of the chat went unaccounted for")
	}
}

// TestReadComposerBubbleStockReportsInconclusiveForAnUnreadableChat pins the
// field that decides whether a chat keeps its record. Rows and content alone
// cannot distinguish a chat that holds nothing from one whose rows could not be
// read, and the scan rebuilds from discovery, so the difference is a conversation
// staying in the index or leaving it.
func TestReadComposerBubbleStockReportsInconclusiveForAnUnreadableChat(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c-unreadable:u-1', NULL)`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c-empty:e-1', '{"_v":3,"type":1,"bubbleId":"e-1","text":"   "}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c-full:f-1', '{"_v":3,"type":1,"bubbleId":"f-1","text":"the question"}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	for _, testCase := range []struct {
		composerID     string
		wantContent    bool
		wantConclusive bool
	}{
		{composerID: "c-unreadable", wantContent: false, wantConclusive: false},
		{composerID: "c-empty", wantContent: false, wantConclusive: true},
		{composerID: "c-full", wantContent: true, wantConclusive: true},
	} {
		stock, err := ReadComposerBubbleStock(context.Background(), readonly, testCase.composerID)
		if err != nil {
			t.Fatalf("ReadComposerBubbleStock(%q) returned error: %v", testCase.composerID, err)
		}
		if stock.HasContent != testCase.wantContent || stock.Conclusive != testCase.wantConclusive {
			t.Fatalf("stock(%q) = %+v, want content %v and conclusive %v",
				testCase.composerID, stock, testCase.wantContent, testCase.wantConclusive)
		}
	}
}

// TestAssembleTreatsAJSONNullToolFieldAsNoToolCall covers a member Cursor set to
// JSON null. `json_type` answers the string "null" for it, so reading that as a
// tool call gives the digest content the decoder maps to nil, and the chat is
// admitted for content it does not have and then streams nothing.
func TestAssembleTreatsAJSONNullToolFieldAsNoToolCall(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c-null-tool:b-1', '{"_v":3,"type":2,"bubbleId":"b-1","text":"","toolFormerData":null}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	stock, err := ReadComposerBubbleStock(context.Background(), readonly, "c-null-tool")
	if err != nil {
		t.Fatalf("ReadComposerBubbleStock returned error: %v", err)
	}
	if stock.HasContent {
		t.Fatal("a chat whose only row has an empty text and a null tool field reported content, so it is admitted and then streams nothing")
	}
}

// TestAssembleAppendsALateAdditionAfterATrailingUndatedHeaderBubble covers a gap
// with no dated header bubble at or after it. That gap is open ended, and the
// rule is that a bubble no gap brackets lands after the whole header order.
func TestAssembleAppendsALateAdditionAfterATrailingUndatedHeaderBubble(t *testing.T) {
	header := headerFixture("c", "b-1", "b-undated")
	stored := storedFixtureOf(
		bubbleFixture("b-1", BubbleTypeUser, "2026-05-06T05:00:10.000Z", "first"),
		bubbleFixture("b-undated", BubbleTypeAssistant, "", "undated trailing header bubble"),
		bubbleFixture("b-late", BubbleTypeAssistant, "2026-05-06T05:09:00.000Z", "late addition"),
	)

	got := stored.assembledText(t, header)
	if got != "first|undated trailing header bubble|late addition" {
		t.Fatalf("assembled = %q, want the late addition after the whole header order", got)
	}
}

// TestAssembleTreatsAnUnparsableTimestampAsUndated keeps ordering and rendering
// agreeing about one field. The message mapper treats a write time it cannot
// parse as unknown, so ordering must not give the same value a definite interior
// position.
func TestAssembleTreatsAnUnparsableTimestampAsUndated(t *testing.T) {
	header := headerFixture("c", "b-1", "b-2")
	stored := storedFixtureOf(
		bubbleFixture("b-1", BubbleTypeUser, "2026-05-06T05:00:10.000Z", "first"),
		bubbleFixture("b-2", BubbleTypeAssistant, "2026-05-06T05:00:30.000Z", "second"),
		bubbleFixture("b-malformed", BubbleTypeAssistant, "2026-05-06T05:00:20.badZ", "malformed timestamp"),
	)

	got := stored.assembledText(t, header)
	if got != "first|second|malformed timestamp" {
		t.Fatalf("assembled = %q, want the unparsable timestamp treated as undated and appended", got)
	}
}

func TestStreamComposerBubblesLogsAnUnparsableStoredTimestamp(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:c:b-malformed', '{"_v":3,"type":2,"bubbleId":"b-malformed","text":"the answer","createdAt":"2026-05-06T05:00:20.badZ"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:c', '{"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b-malformed","type":2}]}')`,
	})
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	header, found, err := ReadComposerHeader(context.Background(), readonly, "c")
	if err != nil || !found {
		t.Fatalf("ReadComposerHeader found = %v, err = %v", found, err)
	}

	var logOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	err = StreamComposerBubbles(context.Background(), readonly, "c", header, func(Bubble) bool {
		return true
	})
	if err != nil {
		t.Fatalf("StreamComposerBubbles returned error: %v", err)
	}
	if !strings.Contains(logOutput.String(), "providers.cursor.store.bubble_timestamp_unparsed") {
		t.Fatalf("log = %s, want the authoritative unparsable timestamp reported", logOutput.String())
	}
}
