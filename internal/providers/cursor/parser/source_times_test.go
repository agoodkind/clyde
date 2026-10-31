package parser_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/providers/cursor/parser"
	"goodkind.io/clyde/internal/transcript"
)

const (
	sourceTimesConversationID = "77777777-7777-4777-8777-777777777777"
	earlyStopDeadline         = 15 * time.Second
)

const (
	sourceRootComposerID = "88888888-8888-4888-8888-888888888888"
	nativeComposerID     = "99999999-9999-4999-8999-999999999999"
	nativeGitCommand     = "git status --short"
	nativeTestCommand    = "go test ./..."
)

// nativeStateStatements builds SQL statements for native message blobs and state.
func nativeStateStatements(composerID string, headers string, messages []string) []string {
	var state []byte
	statements := make([]string, 0, len(messages)+1)
	for _, message := range messages {
		digest := sha256.Sum256([]byte(message))
		state = append(state, 0x0a, byte(len(digest)))
		state = append(state, digest[:]...)
		statements = append(statements, "INSERT INTO cursorDiskKV(key, value) VALUES ('agentKv:blob:"+hex.EncodeToString(digest[:])+"', X'"+hex.EncodeToString([]byte(message))+"')")
	}
	encoded := "~" + base64.StdEncoding.EncodeToString(state)
	statements = append(statements, `INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:`+composerID+`', '{"composerId":"`+composerID+`","name":"Native","createdAt":1779267600000,"lastUpdatedAt":1779268000000,"conversationState":"`+encoded+`","fullConversationHeadersOnly":[`+headers+`]}')`)
	return statements
}

func nativeCall(id string, name string, command string) string {
	return `{"type":"tool-call","toolCallId":"` + id + `","toolName":"` + name + `","args":{"command":"` + command + `"}}`
}

func nativeToolBubble(bubbleID string, created string, callID string, rawArgs string, serverID string) string {
	prefix := "bubbleId:" + nativeComposerID + ":"
	return `INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + bubbleID + `', '{"_v":3,"type":2,"bubbleId":"` + bubbleID + `","text":"","createdAt":"` + created + `","serverBubbleId":"` + serverID + `","toolFormerData":{"name":"run_terminal_command_v2","rawArgs":"` + rawArgs + `","status":"completed","toolCallId":"` + callID + `"}}')`
}

func writeNativeFixture(t *testing.T, secondCallName string) string {
	t.Helper()
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)
	transcriptPath := writeSourceTimesTranscript(t, projectsDir, "Users-alice-native", nativeComposerID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"check the tree"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"` + nativeGitCommand + `"}},{"type":"tool_use","name":"Shell","input":{"command":"` + nativeTestCommand + `"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"` + nativeTestCommand + `"}}]}}`,
	})
	prefix := "bubbleId:" + nativeComposerID + ":"
	statements := nativeStateStatements(nativeComposerID, `{"bubbleId":"n1","type":1},{"bubbleId":"n2","type":2},{"bubbleId":"n3","type":2},{"bubbleId":"n4","type":2}`, []string{
		`{"role":"user","content":[{"type":"text","text":"check the tree"}]}`,
		`{"role":"assistant","content":[` + nativeCall("call_git", "Shell", nativeGitCommand) + `,` + nativeCall("call_test_one", secondCallName, nativeTestCommand) + `]}`,
		`{"role":"assistant","content":[` + nativeCall("call_test_two", "Shell", nativeTestCommand) + `]}`,
	})
	statements = append(statements,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('`+prefix+`n1', '{"_v":3,"type":1,"bubbleId":"n1","text":"check the tree","createdAt":"2026-05-20T10:00:00.000Z"}')`,
		nativeToolBubble("n2", "2026-05-20T10:01:00.000Z", "call_git", "", ""),
		nativeToolBubble("n3", "2026-05-20T10:02:00.000Z", "call_test_one", "", ""),
		nativeToolBubble("n4", "2026-05-20T10:04:00.000Z", "call_test_two", "", ""),
		nativeToolBubble("copy-git", "2026-05-20T12:00:00.000Z", "aaaaaaaa-0000-4000-8000-000000000001", `{\"command\":\"`+nativeGitCommand+`\"}`, "server-git"),
	)
	createSourceTimesGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), statements)
	return transcriptPath
}

func toolTime(t *testing.T, messages []transcript.Message, message int, tool int) time.Time {
	t.Helper()
	if len(messages) <= message || len(messages[message].Tools) <= tool {
		t.Fatalf("tool call is absent; message index, tool index, messages: %d %d: %+v", message, tool, messages)
	}
	stamp := messages[message].Tools[tool].Timestamp
	if stamp == nil {
		return time.Time{}
	}
	return *stamp
}

// TestTranscriptToolCallsTakeTheTimesOfTheirNativeIdentities checks each tool time
// and the grouped message time.
func TestTranscriptToolCallsTakeTheTimesOfTheirNativeIdentities(t *testing.T) {
	transcriptPath := writeNativeFixture(t, "Shell")
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("unexpected message count: %d", len(messages))
	}
	wants := []time.Time{
		time.Date(2026, time.May, 20, 10, 1, 0, 0, time.UTC),
		time.Date(2026, time.May, 20, 10, 2, 0, 0, time.UTC),
		time.Date(2026, time.May, 20, 10, 4, 0, 0, time.UTC),
	}
	for tool, want := range wants {
		if got := toolTime(t, messages, 1, tool); !got.Equal(want) {
			t.Fatalf("unexpected tool timestamp; tool index, actual, expected: %d: %v, %v", tool, got, want)
		}
	}
	if !messages[1].Timestamp.IsZero() {
		t.Fatalf("grouped message has a timestamp: %v", messages[1].Timestamp)
	}
}

// TestTranscriptToolCallsStayUndatedWhenTheNativeSequenceDiffers checks tool times
// before and after a native sequence mismatch.
func TestTranscriptToolCallsStayUndatedWhenTheNativeSequenceDiffers(t *testing.T) {
	transcriptPath := writeNativeFixture(t, "run_command")
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	if got, want := toolTime(t, messages, 1, 0), time.Date(2026, time.May, 20, 10, 1, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("unexpected timestamp before mismatch; actual, expected: %v, %v", got, want)
	}
	for _, toolIndex := range []int{1, 2} {
		if got := toolTime(t, messages, 1, toolIndex); !got.IsZero() {
			t.Fatalf("tool after mismatch has a timestamp; tool index, timestamp: %d: %v", toolIndex, got)
		}
	}
	if !messages[1].Timestamp.IsZero() {
		t.Fatalf("message with mismatched tools has a timestamp: %v", messages[1].Timestamp)
	}
}

// TestTranscriptStreamStopsEarlyWithSourceTimes checks that the first dated message
// TestSourceStoreMessagesReportsAnUnreadableNativeState checks that an unreadable
// TestTranscriptCombinedMessageIgnoresACopiedWholeBody checks that a combined
// message stays undated when a copied whole body also matches.
func TestTranscriptCombinedMessageIgnoresACopiedWholeBody(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)
	first := "The first reply part checks the retention rules."
	second := "The second reply part lists the remaining snapshots."
	transcriptPath := writeSourceTimesTranscript(t, projectsDir, "Users-alice-combined", sourceTimesConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"check retention"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"` + first + `"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"` + second + `"}]}}`,
	})
	prefix := "bubbleId:" + sourceTimesConversationID + ":"
	createSourceTimesGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + sourceTimesConversationID + `', '{"composerId":"` + sourceTimesConversationID + `","name":"Combined","createdAt":1779267600000,"lastUpdatedAt":1779268000000,"fullConversationHeadersOnly":[{"bubbleId":"c1","type":1},{"bubbleId":"c2","type":2},{"bubbleId":"c3","type":2}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `c1', '{"_v":3,"type":1,"bubbleId":"c1","text":"check retention","createdAt":"2026-05-20T10:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `c2', '{"_v":3,"type":2,"bubbleId":"c2","text":"` + first + `","createdAt":"2026-05-20T10:01:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `c3', '{"_v":3,"type":2,"bubbleId":"c3","text":"` + second + `","createdAt":"2026-05-20T10:02:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `copy', '{"_v":3,"type":2,"bubbleId":"copy","text":"` + first + `\\n` + second + `","createdAt":"2026-05-20T12:00:00.000Z","serverBubbleId":"server-copy"}')`,
	})
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("cannot stream the transcript: %v", err)
	}
	if len(messages) != 2 || messages[1].Text != first+"\n"+second {
		t.Fatalf("unexpected combined messages; messages: %+v", messages)
	}
	if !messages[1].Timestamp.IsZero() {
		t.Fatalf("combined message has a timestamp; timestamp: %v", messages[1].Timestamp)
	}
}

// // native state returns an error.
func TestSourceStoreMessagesReportsAnUnreadableNativeState(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	createSourceTimesGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + nativeComposerID + `', '{"composerId":"` + nativeComposerID + `","name":"Broken","createdAt":1779267600000,"lastUpdatedAt":1779268000000,"conversationState":"~%%%","fullConversationHeadersOnly":[{"bubbleId":"x1","type":1}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + nativeComposerID + `:x1', '{"_v":3,"type":1,"bubbleId":"x1","text":"broken state","createdAt":"2026-05-20T10:00:00.000Z"}')`,
	})
	record := conversation.Record{
		ID: "cursor:" + nativeComposerID, Provider: providerid.ProviderCursor, NativeID: nativeComposerID, Selector: "",
		Lineage: nil, Origin: conversation.OriginUnspecified, Title: "", TitleUncertain: false, WorkspaceRoot: "",
		ArtifactPath: "", ArtifactKind: "", Model: "", CreatedAt: time.Time{}, UpdatedAt: time.Time{}, SizeBytes: 0, Archived: false, LatestRequestID: "",
	}
	if _, found, err := parser.New().SourceStoreMessages(t.Context(), record, sourceRootLoadOptions()); err == nil {
		t.Fatalf("source store returned without an error for unreadable native state; found: %v", found)
	}
}

// TestTranscriptToolCallsStayUndatedWhenMessageBoundariesDiffer checks tool times
// TestTranscriptToolCallsStayUndatedWhenTheCompleteInputDiffers checks that a
// matching display does not bind calls when their complete inputs differ.
func TestTranscriptToolCallsStayUndatedWhenTheCompleteInputDiffers(t *testing.T) {
	transcriptPath := writeNativeFixture(t, "Shell")
	lines := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"check the tree"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"` + nativeGitCommand + `"}},{"type":"tool_use","name":"Shell","input":{"command":"` + nativeTestCommand + `","working_directory":"/other"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"` + nativeTestCommand + `"}}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	if messages[1].Tools[1].Display != nativeTestCommand {
		t.Fatalf("unexpected tool display, actual: %q", messages[1].Tools[1].Display)
	}
	if got, want := toolTime(t, messages, 1, 0), time.Date(2026, time.May, 20, 10, 1, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("unexpected first tool time, actual, expected: %v, %v", got, want)
	}
	for _, toolIndex := range []int{1, 2} {
		if got := toolTime(t, messages, 1, toolIndex); !got.IsZero() {
			t.Fatalf("tool has a timestamp after the input mismatch, tool index, actual: %d: %v", toolIndex, got)
		}
	}
}

// // when a transcript message starts within a native message.
func TestTranscriptToolCallsStayUndatedWhenMessageBoundariesDiffer(t *testing.T) {
	transcriptPath := writeNativeFixture(t, "Shell")
	lines := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"check the tree"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"` + nativeGitCommand + `"}}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"now the tests"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"` + nativeTestCommand + `"}},{"type":"tool_use","name":"Shell","input":{"command":"` + nativeTestCommand + `"}}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("unexpected message count: %d", len(messages))
	}
	if got, want := toolTime(t, messages, 1, 0), time.Date(2026, time.May, 20, 10, 1, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("unexpected timestamp before boundary mismatch; actual, expected: %v, %v", got, want)
	}
	for _, toolIndex := range []int{0, 1} {
		if got := toolTime(t, messages, 3, toolIndex); !got.IsZero() {
			t.Fatalf("tool after boundary mismatch has a timestamp; tool index, timestamp: %d: %v", toolIndex, got)
		}
	}
}

// is available before the FIFO writer closes.
func TestTranscriptStreamStopsEarlyWithSourceTimes(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)
	prefix := "bubbleId:" + sourceTimesConversationID + ":"
	createSourceTimesGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + sourceTimesConversationID + `', '{"composerId":"` + sourceTimesConversationID + `","name":"Stream","createdAt":1779267600000,"lastUpdatedAt":1779268000000,"fullConversationHeadersOnly":[{"bubbleId":"f1","type":1}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `f1', '{"_v":3,"type":1,"bubbleId":"f1","text":"stream question","createdAt":"2026-05-20T10:00:00.000Z"}')`,
	})
	transcriptPath := filepath.Join(projectsDir, "Users-alice-stream", "agent-transcripts", sourceTimesConversationID, sourceTimesConversationID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("create transcript directory: %v", err)
	}
	if err := syscall.Mkfifo(transcriptPath, 0o600); err != nil {
		t.Fatalf("create transcript FIFO: %v", err)
	}
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(transcriptPath, os.O_WRONLY, 0)
		if err != nil {
			writerDone <- err
			return
		}
		_, writeErr := writer.WriteString(`{"role":"user","message":{"content":[{"type":"text","text":"stream question"}]}}` + "\n" +
			`{"role":"assistant","message":{"content":[{"type":"text","text":"first answer"}]}}` + "\n")
		<-release
		writerDone <- errors.Join(writeErr, writer.Close())
	}()
	defer func() {
		close(release)
		<-writerDone
	}()
	result := make(chan transcript.Message, 1)
	failure := make(chan error, 1)
	go func() {
		for message, err := range parser.New().Stream(transcriptPath, sourceRootLoadOptions()) {
			if err != nil {
				failure <- err
				return
			}
			result <- message
			return
		}
		failure <- errors.New("transcript stream returned without a message")
	}()
	select {
	case message := <-result:
		if want := time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC); !message.Timestamp.Equal(want) {
			t.Fatalf("unexpected first message timestamp; actual, expected: %v, %v", message.Timestamp, want)
		}
	case err := <-failure:
		t.Fatalf("stream transcript: %v", err)
	case <-time.After(earlyStopDeadline):
		t.Fatal("transcript stream waited for the FIFO writer to close")
	}
}

func writeSourceRootStore(t *testing.T, root string, questionTime string, answerTime string) {
	t.Helper()
	prefix := "bubbleId:" + sourceRootComposerID + ":"
	createSourceTimesGlobalDB(t, filepath.Join(root, "globalStorage", "state.vscdb"), []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + sourceRootComposerID + `', '{"composerId":"` + sourceRootComposerID + `","name":"Roots","createdAt":1779267600000,"lastUpdatedAt":1779268000000,"fullConversationHeadersOnly":[{"bubbleId":"r1","type":1},{"bubbleId":"r2","type":2}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `r1', '{"_v":3,"type":1,"bubbleId":"r1","text":"which root wins","createdAt":"` + questionTime + `"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `r2', '{"_v":3,"type":2,"bubbleId":"r2","text":"the record root wins","createdAt":"` + answerTime + `"}')`,
	})
}

func sourceRootLoadOptions() conversation.LoadOptions {
	return conversation.LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: false, IncludeToolOutputs: false}
}

func streamSourceRootTranscript(t *testing.T, transcriptPath string) []time.Time {
	t.Helper()
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	times := make([]time.Time, 0, len(messages))
	for _, message := range messages {
		times = append(times, message.Timestamp)
	}
	return times
}

// TestComposerSourceRootFollowsTheRecordAndRefusesConflicts writes the same
// composer text with different times to two Cursor global databases. For both
// root orders, it checks that a JSONL transcript keeps zero times and that
// SourceStoreMessages returns the second root's times for a record from that
// root. It then checks that roots with equal messages and times date the
// transcript.
func TestComposerSourceRootFollowsTheRecordAndRefusesConflicts(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	rootSame := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)
	writeSourceRootStore(t, rootA, "2026-05-20T10:00:00.000Z", "2026-05-20T10:01:00.000Z")
	writeSourceRootStore(t, rootB, "2026-05-21T11:00:00.000Z", "2026-05-21T11:01:00.000Z")
	writeSourceRootStore(t, rootSame, "2026-05-20T10:00:00.000Z", "2026-05-20T10:01:00.000Z")
	transcriptPath := writeSourceTimesTranscript(t, projectsDir, "Users-alice-source-roots", sourceRootComposerID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"which root wins"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"the record root wins"}]}}`,
	})
	recordB := conversation.Record{
		ID: "cursor:" + sourceRootComposerID, Provider: providerid.ProviderCursor, NativeID: sourceRootComposerID, Selector: "",
		Lineage: nil, Origin: conversation.OriginUnspecified, Title: "", TitleUncertain: false, WorkspaceRoot: "",
		ArtifactPath: parser.BuildVirtualPath(parser.RootHash(rootB), parser.VirtualKindComposer, sourceRootComposerID),
		ArtifactKind: "", Model: "", CreatedAt: time.Time{}, UpdatedAt: time.Time{}, SizeBytes: 0, Archived: false, LatestRequestID: "",
	}
	wantB := time.Date(2026, time.May, 21, 11, 0, 0, 0, time.UTC)
	for _, order := range [][]string{{rootA, rootB}, {rootB, rootA}} {
		t.Setenv("CLYDE_CURSOR_DATA_DIRS", strings.Join(order, string(os.PathListSeparator)))
		for position, stamp := range streamSourceRootTranscript(t, transcriptPath) {
			if !stamp.IsZero() {
				t.Fatalf("transcript has a time when roots disagree; root order, message position, time: %v, %d: %v", order, position, stamp)
			}
		}
		messages, found, err := parser.New().SourceStoreMessages(t.Context(), recordB, sourceRootLoadOptions())
		if err != nil || !found || len(messages.Messages) == 0 || !messages.Messages[0].Timestamp.Equal(wantB) {
			t.Fatalf("source messages did not come from the record's root; root order, found, error, messages: %v: %v, %v, %+v", order, found, err, messages)
		}
	}
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", strings.Join([]string{rootA, rootSame}, string(os.PathListSeparator)))
	times := streamSourceRootTranscript(t, transcriptPath)
	if len(times) != 2 || !times[0].Equal(time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("roots with equal messages did not date the transcript; times: %v", times)
	}
}

// TestTranscriptMessagesTakeTimesFromTheComposerStore checks source timestamp matching.
// It also checks existing, ambiguous, and unmatched transcript timestamps.
func TestTranscriptMessagesTakeTimesFromTheComposerStore(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	transcriptPath := writeSourceTimesTranscript(t, projectsDir, "Users-alice-source-times", sourceTimesConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"first question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"repeated answer"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>2026-05-20T09:00:00Z</timestamp>\n<user_query>\nsecond question\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"read_file","input":{"path":"main.go"}}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"third question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"same second reply"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"listed last but dated first"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"answer the store never saved"}]}}`,
	})

	prefix := "bubbleId:" + sourceTimesConversationID + ":"
	createSourceTimesGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + sourceTimesConversationID + `', '{"composerId":"` + sourceTimesConversationID + `","name":"Source Times","createdAt":1779267600000,"lastUpdatedAt":1779268000000,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2},{"bubbleId":"b4","type":1},{"bubbleId":"b5","type":2},{"bubbleId":"b6","type":1},{"bubbleId":"b7","type":2},{"bubbleId":"b8","type":1}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b1', '{"_v":3,"type":1,"bubbleId":"b1","text":"first question","createdAt":"2026-05-20T10:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b2', '{"_v":3,"type":2,"bubbleId":"b2","text":"repeated answer","createdAt":"2026-05-20T10:01:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b3', '{"_v":3,"type":2,"bubbleId":"b3","text":"","createdAt":"2026-05-20T10:02:00.000Z","toolFormerData":{"name":"read_file","rawArgs":"{\"path\":\"main.go\"}","status":"completed"}}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b4', '{"_v":3,"type":1,"bubbleId":"b4","text":"third question","createdAt":"2026-05-20T10:05:00.100Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b5', '{"_v":3,"type":2,"bubbleId":"b5","text":"same second reply","createdAt":"2026-05-20T10:05:00.900Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b6', '{"_v":3,"type":1,"bubbleId":"b6","text":"listed last but dated first","createdAt":"2026-05-20T08:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b7', '{"_v":3,"type":2,"bubbleId":"b7","text":"repeated answer","createdAt":"2026-05-20T10:06:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b8', '{"_v":3,"type":1,"bubbleId":"b8","text":"a turn the transcript lacks","createdAt":"2026-05-20T10:07:00.000Z"}')`,
	})

	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	want := []time.Time{
		time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC),
		{},
		time.Date(2026, time.May, 20, 9, 0, 0, 0, time.UTC),
		{},
		time.Date(2026, time.May, 20, 10, 5, 0, 100_000_000, time.UTC),
		time.Date(2026, time.May, 20, 10, 5, 0, 900_000_000, time.UTC),
		time.Date(2026, time.May, 20, 8, 0, 0, 0, time.UTC),
		{},
	}
	if len(messages) != len(want) {
		t.Fatalf("message count differs from expected %d, %d", len(messages), len(want))
	}
	for position, message := range messages {
		if !message.Timestamp.Equal(want[position]) {
			t.Errorf("message timestamp differs from expected at position %d (%s %q): %v, %v", position, message.Role, message.Text, message.Timestamp, want[position])
		}
	}
}

// TestTranscriptWithoutAComposerRecordKeepsZeroTimes checks timestamps without a composer record.
func TestTranscriptWithoutAComposerRecordKeepsZeroTimes(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)
	transcriptPath := writeSourceTimesTranscript(t, projectsDir, "Users-alice-source-times", sourceTimesConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"first question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"an answer"}]}}`,
	})
	createSourceTimesGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), nil)

	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("message count differs from expected %d, %d", len(messages), 2)
	}
	for position, message := range messages {
		if !message.Timestamp.IsZero() {
			t.Errorf("message has an unexpected timestamp at position %d: %v", position, message.Timestamp)
		}
	}
}

func writeSourceTimesTranscript(t *testing.T, root string, projectKey string, conversationID string, lines []string) string {
	t.Helper()
	path := filepath.Join(root, projectKey, "agent-transcripts", conversationID, conversationID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll transcript dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile transcript: %v", err)
	}
	return path
}

func createSourceTimesGlobalDB(t *testing.T, dbPath string, extra []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open global db: %v", err)
	}
	statements := append([]string{"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)", "CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)"}, extra...)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("exec global statement %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close global db: %v", err)
	}
}
