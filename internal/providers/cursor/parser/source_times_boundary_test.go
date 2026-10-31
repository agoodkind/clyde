package parser_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	boundaryFirstRequest  = "first request"
	boundarySecondRequest = "second request"
	boundaryInjected      = "<system_reminder>check the rules</system_reminder>"
	boundaryLargeNumber   = "12345678901234567890"
	boundaryOtherNumber   = "12345678901234567891"
)

type escapedOptions struct {
	Label string      `json:"label"`
	Depth json.Number `json:"depth"`
}

type escapedArgs struct {
	Command string         `json:"command"`
	Options escapedOptions `json:"options"`
}

type reorderedOptions struct {
	Depth json.Number `json:"depth"`
	Label string      `json:"label"`
}

type reorderedArgs struct {
	Options reorderedOptions `json:"options"`
	Command string           `json:"command"`
}

type depthArgs struct {
	Command string      `json:"command"`
	Depth   json.Number `json:"depth"`
}

// boundaryRoots creates temporary data and transcript roots and writes the transcript.
func boundaryRoots(t *testing.T, lines []string) (string, string) {
	t.Helper()
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)
	return rootDir, writeSourceTimesTranscript(t, projectsDir, "Users-alice-boundary", nativeComposerID, lines)
}

func boundaryToolTimes(t *testing.T, transcriptPath string) [][]time.Time {
	t.Helper()
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("cannot stream transcript: %v", err)
	}
	times := make([][]time.Time, 0)
	for messageIndex, message := range messages {
		if len(message.Tools) == 0 {
			continue
		}
		row := make([]time.Time, 0, len(message.Tools))
		for toolIndex := range message.Tools {
			row = append(row, toolTime(t, messages, messageIndex, toolIndex))
		}
		times = append(times, row)
	}
	return times
}

func requireToolTimes(t *testing.T, got [][]time.Time, want [][]time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tool message timestamps differ; actual, expected: %v, %v", got, want)
	}
	for message := range want {
		if len(got[message]) != len(want[message]) {
			t.Fatalf("tool counts differ; message index, actual times, expected times: %d: %v, %v", message, got[message], want[message])
		}
		for tool := range want[message] {
			if !got[message][tool].Equal(want[message][tool]) {
				t.Fatalf("tool timestamp differs; message index, tool index, actual, expected: %d %d: %v, %v", message, tool, got[message][tool], want[message][tool])
			}
		}
	}
}

func minuteAfterTen(minute int) time.Time {
	return time.Date(2026, time.May, 20, 10, minute, 0, 0, time.UTC)
}

func boundaryBubbles() []fixtureBubble {
	return []fixtureBubble{
		userBubble("u1", boundaryFirstRequest, "2026-05-20T10:00:00.000Z"),
		toolBubble("t1", "2026-05-20T10:01:00.000Z", "call_a"),
		userBubble("u2", boundarySecondRequest, "2026-05-20T10:02:00.000Z"),
		toolBubble("t2", "2026-05-20T10:03:00.000Z", "call_b"),
	}
}

func writeBoundaryStore(t *testing.T, rootDir string, messages []fixtureNativeMessage, omitted map[int]bool, bubbles []fixtureBubble) {
	t.Helper()
	writeNativeStore(t, rootDir, nativeStoreFixture{composerID: nativeComposerID, messages: messages, omitted: omitted, bubbles: bubbles})
}

// TestTranscriptToolCallsStayUndatedAcrossANativeUserTurn checks that a native
// user turn prevents dating the second call in one transcript message.
func TestTranscriptToolCallsStayUndatedAcrossANativeUserTurn(t *testing.T) {
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, boundaryFirstRequest),
		toolLine(t, commandArgs(t, nativeGitCommand, ""), commandArgs(t, nativeTestCommand, "")),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(boundaryFirstRequest),
		assistantMessage(callPart("call_a", commandArgs(t, nativeGitCommand, ""))),
		userMessage(boundarySecondRequest),
		assistantMessage(callPart("call_b", commandArgs(t, nativeTestCommand, ""))),
	}, nil, boundaryBubbles())
	requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{minuteAfterTen(1), {}}})
}

// TestTranscriptToolCallsStayUndatedAfterAnUnmatchedTranscriptUserTurn checks that
// a transcript user turn without a native counterpart prevents dating the next call.
func TestTranscriptToolCallsStayUndatedAfterAnUnmatchedTranscriptUserTurn(t *testing.T) {
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, boundaryFirstRequest),
		toolLine(t, commandArgs(t, nativeGitCommand, "")),
		userLine(t, boundarySecondRequest),
		toolLine(t, commandArgs(t, nativeTestCommand, "")),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(boundaryFirstRequest),
		assistantMessage(callPart("call_a", commandArgs(t, nativeGitCommand, ""))),
		assistantMessage(callPart("call_b", commandArgs(t, nativeTestCommand, ""))),
	}, nil, boundaryBubbles())
	requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{minuteAfterTen(1)}, {{}}})
}

// TestTranscriptToolCallsBindAcrossInjectedContext checks that injected native
// context does not prevent matching tool calls.
func TestTranscriptToolCallsBindAcrossInjectedContext(t *testing.T) {
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, boundaryFirstRequest),
		toolLine(t, commandArgs(t, nativeGitCommand, ""), commandArgs(t, nativeTestCommand, "")),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(boundaryFirstRequest),
		assistantMessage(callPart("call_a", commandArgs(t, nativeGitCommand, ""))),
		userMessage(boundaryInjected),
		assistantMessage(callPart("call_b", commandArgs(t, nativeTestCommand, ""))),
	}, nil, boundaryBubbles())
	requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{minuteAfterTen(1), minuteAfterTen(3)}})
}

// TestTranscriptToolCallsCountARepeatedUserReference checks that a repeated native
// user reference marks a user turn between calls.
func TestTranscriptToolCallsCountARepeatedUserReference(t *testing.T) {
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, boundaryFirstRequest),
		toolLine(t, commandArgs(t, nativeGitCommand, "")),
		userLine(t, boundaryFirstRequest),
		toolLine(t, commandArgs(t, nativeTestCommand, "")),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(boundaryFirstRequest),
		assistantMessage(callPart("call_a", commandArgs(t, nativeGitCommand, ""))),
		userMessage(boundaryFirstRequest),
		assistantMessage(callPart("call_b", commandArgs(t, nativeTestCommand, ""))),
	}, nil, boundaryBubbles())
	requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{minuteAfterTen(1)}, {minuteAfterTen(3)}})
}

// escapedNativeArgs returns JSON arguments with escaped characters.
func escapedNativeArgs(t *testing.T) json.RawMessage {
	t.Helper()
	plain := mustMarshal(t)(json.Marshal(escapedArgs{Command: nativeGitCommand, Options: escapedOptions{Label: "x", Depth: boundaryLargeNumber}}))
	escaped := bytes.Replace(plain, []byte("status"), []byte("st"+jsonUnicodeEscape('a')+"tus"), 1)
	escaped = bytes.Replace(escaped, []byte(`"x"`), []byte(`"`+jsonUnicodeEscape('x')+`"`), 1)
	if bytes.Equal(escaped, plain) || !json.Valid(escaped) {
		t.Fatalf("escaped arguments did not change or are invalid JSON; arguments: %s", escaped)
	}
	return escaped
}

// TestTranscriptToolCallsMatchEquivalentEscapedInputs checks that reordered keys
// and equivalent string escapes match in tool inputs.
func TestTranscriptToolCallsMatchEquivalentEscapedInputs(t *testing.T) {
	transcriptInput := mustMarshal(t)(json.Marshal(reorderedArgs{Options: reorderedOptions{Depth: boundaryLargeNumber, Label: "x"}, Command: nativeGitCommand}))
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, boundaryFirstRequest),
		toolLine(t, transcriptInput),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(boundaryFirstRequest),
		assistantMessage(callPart("call_a", escapedNativeArgs(t))),
	}, nil, boundaryBubbles()[:2])
	requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{minuteAfterTen(1)}})
}

// TestTranscriptToolCallsRejectADifferentLargeNumber checks that different large
// number literals prevent a tool call match.
func TestTranscriptToolCallsRejectADifferentLargeNumber(t *testing.T) {
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, boundaryFirstRequest),
		toolLine(t, mustMarshal(t)(json.Marshal(depthArgs{Command: nativeGitCommand, Depth: boundaryOtherNumber}))),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(boundaryFirstRequest),
		assistantMessage(callPart("call_a", mustMarshal(t)(json.Marshal(depthArgs{Command: nativeGitCommand, Depth: boundaryLargeNumber})))),
	}, nil, boundaryBubbles()[:2])
	requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{{}}})
}

// TestTranscriptToolCallsStayUndatedWhenRootsDifferInFullInput checks that a
// difference in an undisplayed argument prevents dating across roots.
func TestTranscriptToolCallsStayUndatedWhenRootsDifferInFullInput(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	rootSame := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)
	transcriptPath := writeSourceTimesTranscript(t, projectsDir, "Users-alice-roots-input", nativeComposerID, []string{
		userLine(t, boundaryFirstRequest),
		toolLine(t, commandArgs(t, nativeGitCommand, "")),
	})
	for _, root := range []struct {
		dir  string
		args json.RawMessage
	}{
		{dir: rootA, args: commandArgs(t, nativeGitCommand, "")},
		{dir: rootB, args: commandArgs(t, nativeGitCommand, "/other")},
		{dir: rootSame, args: commandArgs(t, nativeGitCommand, "")},
	} {
		writeBoundaryStore(t, root.dir, []fixtureNativeMessage{userMessage(boundaryFirstRequest), assistantMessage(callPart("call_a", root.args))}, nil, boundaryBubbles()[:2])
	}
	for _, order := range [][]string{{rootA, rootB}, {rootB, rootA}} {
		t.Setenv("CLYDE_CURSOR_DATA_DIRS", strings.Join(order, string(os.PathListSeparator)))
		requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{{}}})
	}
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", strings.Join([]string{rootA, rootSame}, string(os.PathListSeparator)))
	requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{minuteAfterTen(1)}})
}

// TestSourceStoreMessagesReportsAMissingNativeRecord checks the error for a missing
// native record and the timestamps returned by transcript streaming.
func TestSourceStoreMessagesReportsAMissingNativeRecord(t *testing.T) {
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, boundaryFirstRequest),
		toolLine(t, commandArgs(t, nativeGitCommand, "")),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(boundaryFirstRequest),
		assistantMessage(callPart("call_a", commandArgs(t, nativeGitCommand, ""))),
	}, map[int]bool{1: true}, boundaryBubbles()[:2])
	record := conversation.Record{
		ID: "cursor:" + nativeComposerID, Provider: providerid.ProviderCursor, NativeID: nativeComposerID, Selector: "",
		Lineage: nil, Origin: conversation.OriginUnspecified, Title: "", TitleUncertain: false, WorkspaceRoot: "",
		ArtifactPath: "", ArtifactKind: "", Model: "", CreatedAt: time.Time{}, UpdatedAt: time.Time{}, SizeBytes: 0, Archived: false, LatestRequestID: "",
	}
	if _, found, err := parser.New().SourceStoreMessages(t.Context(), record, sourceRootLoadOptions()); err == nil {
		t.Fatalf("source store did not report a missing native record; found: %v", found)
	}
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("cannot stream transcript with a missing native record: %v", err)
	}
	if !messages[0].Timestamp.Equal(minuteAfterTen(0)) {
		t.Fatalf("transcript text timestamp differs from expected 10:00 UTC; actual: %v", messages[0].Timestamp)
	}
	if got := toolTime(t, messages, 1, 0); !got.IsZero() {
		t.Fatalf("tool call has a timestamp despite a missing native record; timestamp: %v", got)
	}
}
