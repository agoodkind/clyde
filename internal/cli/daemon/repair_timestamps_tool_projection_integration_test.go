//go:build milvusintegration

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const (
	projectionShellName       = "Shell"
	projectionGitArgs         = `{"command":"` + userBoundaryGit + `"}`
	projectionTestArgs        = `{"command":"` + userBoundaryTest + `"}`
	projectionGitTokens       = projectionShellName + "\ngit\n" + projectionGitArgs
	projectionTestTokens      = projectionShellName + "\ngo\n" + projectionTestArgs
	projectionGitBase         = projectionShellName + "\ngit\n" + userBoundaryGit
	projectionTestBase        = projectionShellName + "\ngo\n" + userBoundaryTest
	projectionFirstDate       = "2026-05-20T10:01:00.000Z"
	projectionSecondDate      = "2026-05-20T10:03:00.000Z"
	projectionUserDate        = "2026-05-20T10:00:00.000Z"
	projectionNextDate        = "2026-05-20T10:02:00.000Z"
	projectionQuestion        = "Check the backup tree first."
	projectionFollowUp        = "Now run the tests."
	projectionReply           = "The backup tree is clean."
	projectionOtherText       = "A different stored question."
	projectionRepeatedCommand = "git diff --stat"
	projectionRepeatedBase    = projectionShellName + "\ngit\n" + projectionRepeatedCommand
)

// projectionMessage is one native message with its bubble date.
type projectionMessage struct {
	role    string
	text    string
	callID  string
	command string
	created string
}

// projectionRow is one stored row after the test rewrites its path and content.
type projectionRow struct {
	path    string
	content string
	role    string
}

func projectionHome(t *testing.T, messages []projectionMessage) (string, string, string) {
	t.Helper()
	return projectionHomeWithTranscript(t, messages, messages)
}

func projectionHomeWithTranscript(t *testing.T, messages []projectionMessage, transcriptMessages []projectionMessage) (string, string, string) {
	t.Helper()
	address, home := correctionHome(t)
	writeProjectionSources(t, filepath.Join(home, "cursor-data"), filepath.Join(home, "cursor-projects"), messages, transcriptMessages)
	return address, home, "cursor:" + correctionComposerID
}

func writeProjectionSources(t *testing.T, dataRoot string, projects string, messages []projectionMessage, transcriptMessages []projectionMessage) {
	t.Helper()
	marshal := combinedCopyJSON(t)
	native := make([]userBoundaryNativeMessage, 0, len(messages))
	bubbles := make([]userBoundaryBubble, 0, len(messages))
	for index, message := range messages {
		bubble := userBoundaryBubble{Version: 3, Type: 2, BubbleID: fmt.Sprintf("b%d", index), Text: message.text, CreatedAt: message.created, ToolFormerData: nil}
		if message.role == "user" {
			bubble.Type = 1
		}
		if message.callID == "" {
			native = append(native, userBoundaryNativeMessage{Role: message.role, Content: []userBoundaryNativePart{{Type: "text", Text: message.text, ToolCallID: "", ToolName: "", Args: nil}}})
		} else {
			args := json.RawMessage(marshal(json.Marshal(userBoundaryArgs{Command: message.command})))
			native = append(native, userBoundaryNativeMessage{Role: message.role, Content: []userBoundaryNativePart{{Type: "tool-call", Text: "", ToolCallID: message.callID, ToolName: projectionShellName, Args: args}}})
			bubble.ToolFormerData = &userBoundaryToolFormer{Name: "run_terminal_command_v2", RawArgs: "", Status: "completed", ToolCallID: message.callID}
		}
		bubbles = append(bubbles, bubble)
	}
	lines := make([]string, 0, len(transcriptMessages))
	for _, message := range transcriptMessages {
		var line userBoundaryLine
		line.Role = message.role
		if message.callID == "" {
			line.Message.Content = []userBoundaryLinePart{{Type: "text", Text: message.text, Name: "", Input: nil}}
		} else {
			args := json.RawMessage(marshal(json.Marshal(userBoundaryArgs{Command: message.command})))
			line.Message.Content = []userBoundaryLinePart{{Type: "tool_use", Text: "", Name: projectionShellName, Input: args}}
		}
		lines = append(lines, marshal(json.Marshal(line)))
	}
	writeNativeComposerStore(t, dataRoot, correctionComposerID, native, bubbles, nil)
	transcriptPath := filepath.Join(projects, repairProjectKey, "agent-transcripts", correctionComposerID, correctionComposerID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("failed to create the transcript: %v", err)
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("failed to create the transcript: %v", err)
	}
}

// seedProjectionRows inserts one undated row for each entry and rewrites its path
// and content, which keeps its row ID and vector key.
func seedProjectionRows(t *testing.T, ctx context.Context, address string, conversationID string, rows []projectionRow) {
	t.Helper()
	seedProjectionRowsAt(t, ctx, address, conversationID, rows, time.Time{}.Unix())
}

// seedProjectionRowsAt inserts rows at the supplied timestamp, then rewrites
// their paths and content.
func seedProjectionRowsAt(t *testing.T, ctx context.Context, address string, conversationID string, rows []projectionRow, stamp int64) {
	t.Helper()
	docs := make([]semsearch.SemDoc, 0, len(rows))
	for index, row := range rows {
		doc := repairDoc(conversationID, fmt.Sprintf("seed row %d", index), stamp)
		doc.MessageIndex = int32(index)
		doc.Role = row.role
		docs = append(docs, doc)
	}
	seedCorrectionDocuments(t, ctx, address, conversationID, docs)
	seeded, _ := sharedSnapshot(t, address)
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: "default"})
	if err != nil {
		t.Fatalf("failed to connect to Milvus: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	for index, row := range rows {
		id := correctionRowByContent(t, seeded, fmt.Sprintf("seed row %d", index))
		option := milvusclient.NewColumnBasedInsertOption(vectorsearch.CollectionName(repairCollectionID)).
			WithVarcharColumn("id", []string{id}).
			WithVarcharColumn("relativePath", []string{row.path}).
			WithVarcharColumn("content", []string{row.content}).
			WithPartialUpdate(true)
		if _, err := client.Upsert(ctx, option); err != nil {
			t.Fatalf("failed to update the stored row path and content %s: %v", row.path, err)
		}
	}
	time.Sleep(repairVisibilityWait)
}

// runProjectionRepair runs the public source repair and checks that only dates changed.
func runProjectionRepair(t *testing.T, ctx context.Context, address string, home string) map[string]sharedStoredRow {
	t.Helper()
	before, beforeVectors := sharedSnapshot(t, address)
	if _, err := runCorrectionCommand(t, ctx, address, "--execute", "--limit", "1", "--progress-file", filepath.Join(home, "projection-progress.json")); err != nil {
		t.Fatalf("failed to run source timestamp repair: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	after, afterVectors := sharedSnapshot(t, address)
	if len(after) != len(before) || len(afterVectors) != len(beforeVectors) {
		t.Fatalf("failed to preserve row and vector counts %d, %d, %d, %d", len(after), len(before), len(afterVectors), len(beforeVectors))
	}
	byPath := make(map[string]sharedStoredRow, len(after))
	for id, row := range after {
		prior := before[id]
		if row.relativePath != prior.relativePath || row.content != prior.content || row.vectorKey != prior.vectorKey || row.others != prior.others {
			t.Fatalf("failed to preserve row fields other than timestamps %s: %+v, %+v", id, prior, row)
		}
		byPath[row.relativePath] = row
	}
	return byPath
}

func requireProjectionDate(t *testing.T, rows map[string]sharedStoredRow, path string, created string) {
	t.Helper()
	want, err := time.Parse(time.RFC3339, created)
	if err != nil {
		t.Fatalf("failed to parse the expected date %q: %v", created, err)
	}
	row, found := rows[path]
	if !found || row.timestampNull || row.timestampUnix != want.Unix() {
		t.Fatalf("failed to find a stored row with the expected date %s: %+v, %d", path, row, want.Unix())
	}
}

func requireProjectionUndated(t *testing.T, rows map[string]sharedStoredRow, path string) {
	t.Helper()
	if row, found := rows[path]; !found || !row.timestampNull {
		t.Fatalf("failed to find an undated stored row %s: %+v", path, row)
	}
}

func twoCallMessages(firstCommand string, secondCommand string) []projectionMessage {
	return []projectionMessage{
		{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
		{role: "assistant", text: "", callID: "call_a", command: firstCommand, created: projectionFirstDate},
		{role: "assistant", text: projectionReply, callID: "", command: "", created: projectionNextDate},
		{role: "assistant", text: "", callID: "call_b", command: secondCommand, created: projectionSecondDate},
	}
}

func TestSourceRepairDatesEachStoredRepresentationFromItsOwnProjection(t *testing.T) {
	address, home, conversationID := projectionHome(t, twoCallMessages(userBoundaryGit, userBoundaryTest))
	ctx := context.Background()
	historical := "convtool/" + conversationID + "/1/0"
	grouped := "convtool/" + conversationID + "/1/1"
	seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
		{path: historical + "/tok", content: projectionGitTokens, role: "assistant"},
		{path: historical + "/cmd", content: userBoundaryGit, role: "assistant"},
		{path: historical + "/in/0", content: projectionGitArgs, role: "assistant"},
		{path: grouped, content: projectionTestBase, role: "assistant"},
	})
	rows := runProjectionRepair(t, ctx, address, home)
	requireProjectionDate(t, rows, historical+"/tok", projectionFirstDate)
	requireProjectionDate(t, rows, historical+"/cmd", projectionFirstDate)
	requireProjectionDate(t, rows, historical+"/in/0", projectionFirstDate)
	requireProjectionDate(t, rows, grouped, projectionSecondDate)
}

func TestSourceRepairKeepsContradictoryComponentsUndated(t *testing.T) {
	address, home, conversationID := projectionHome(t, twoCallMessages(userBoundaryGit, userBoundaryTest))
	ctx := context.Background()
	contradictory := "convtool/" + conversationID + "/1/0"
	split := "convtool/" + conversationID + "/3/0"
	seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
		{path: contradictory + "/tok", content: projectionTestTokens, role: "assistant"},
		{path: contradictory + "/cmd", content: userBoundaryGit, role: "assistant"},
		{path: contradictory + "/in/0", content: projectionGitArgs, role: "assistant"},
		{path: split + "/tok", content: projectionTestTokens, role: "assistant"},
		{path: split + "/in/0", content: `{"command":`, role: "assistant"},
		{path: split + "/in/1", content: `"` + userBoundaryTest + `"}`, role: "assistant"},
	})
	rows := runProjectionRepair(t, ctx, address, home)
	for _, path := range []string{contradictory + "/tok", contradictory + "/cmd", contradictory + "/in/0", split + "/tok", split + "/in/0", split + "/in/1"} {
		requireProjectionUndated(t, rows, path)
	}
}

func TestSourceRepairUsesTheOriginalPositionOfASurvivingIdenticalCall(t *testing.T) {
	address, home, conversationID := projectionHome(t, twoCallMessages(userBoundaryGit, userBoundaryGit))
	ctx := context.Background()
	surviving := "convtool/" + conversationID + "/3/0"
	unanchored := "convtool/" + conversationID + "/9/0"
	seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
		{path: surviving, content: projectionGitBase, role: "assistant"},
		{path: unanchored, content: projectionGitBase, role: "assistant"},
	})
	rows := runProjectionRepair(t, ctx, address, home)
	requireProjectionDate(t, rows, surviving, projectionSecondDate)
	requireProjectionUndated(t, rows, unanchored)
}

func TestSourceRepairRequiresAnAssociationForAnotherProjectionUserTurn(t *testing.T) {
	messages := []projectionMessage{
		{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
		{role: "assistant", text: "", callID: "call_a", command: userBoundaryGit, created: projectionFirstDate},
		{role: "assistant", text: projectionReply, callID: "", command: "", created: projectionNextDate},
		{role: "user", text: projectionFollowUp, callID: "", command: "", created: projectionNextDate},
		{role: "assistant", text: "", callID: "call_b", command: userBoundaryTest, created: projectionSecondDate},
	}
	for _, testCase := range []struct {
		name     string
		userText string
		dated    bool
	}{
		{name: "associated", userText: projectionFollowUp, dated: true},
		{name: "unmatched", userText: projectionOtherText, dated: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			address, home, conversationID := projectionHome(t, messages)
			ctx := context.Background()
			first := "convtool/" + conversationID + "/1/0"
			second := "convtool/" + conversationID + "/4/0"
			seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
				{path: first + "/tok", content: projectionGitTokens, role: "assistant"},
				{path: first + "/cmd", content: userBoundaryGit, role: "assistant"},
				{path: first + "/in/0", content: projectionGitArgs, role: "assistant"},
				{path: "conv/" + conversationID + "/2", content: testCase.userText, role: "user"},
				{path: second + "/tok", content: projectionTestTokens, role: "assistant"},
				{path: second + "/cmd", content: userBoundaryTest, role: "assistant"},
				{path: second + "/in/0", content: projectionTestArgs, role: "assistant"},
			})
			rows := runProjectionRepair(t, ctx, address, home)
			requireProjectionDate(t, rows, first+"/tok", projectionFirstDate)
			if testCase.dated {
				requireProjectionDate(t, rows, second+"/tok", projectionSecondDate)
				return
			}
			requireProjectionUndated(t, rows, second+"/tok")
		})
	}
}

func TestSourceRepairUsesTheStoredTranscriptPositionOfACall(t *testing.T) {
	messages := []projectionMessage{
		{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
		{role: "user", text: projectionFollowUp, callID: "", command: "", created: projectionUserDate},
		{role: "assistant", text: "", callID: "call_a", command: userBoundaryGit, created: projectionFirstDate},
		{role: "user", text: projectionOtherText, callID: "", command: "", created: projectionNextDate},
		{role: "assistant", text: "", callID: "call_b", command: userBoundaryTest, created: projectionSecondDate},
	}
	for _, testCase := range []struct {
		name    string
		content string
		dated   bool
	}{
		{name: "same call", content: projectionTestBase, dated: true},
		{name: "different call", content: projectionShellName + "\ngo\ngo vet ./...", dated: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			address, home, conversationID := projectionHome(t, messages)
			ctx := context.Background()
			first := "convtool/" + conversationID + "/1/0"
			second := "convtool/" + conversationID + "/3/0"
			seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
				{path: first, content: projectionGitBase, role: "assistant"},
				{path: "conv/" + conversationID + "/2", content: projectionQuestion, role: "user"},
				{path: second, content: testCase.content, role: "assistant"},
			})
			rows := runProjectionRepair(t, ctx, address, home)
			requireProjectionDate(t, rows, first, projectionFirstDate)
			if testCase.dated {
				requireProjectionDate(t, rows, second, projectionSecondDate)
				return
			}
			requireProjectionUndated(t, rows, second)
		})
	}
}

func TestSourceRepairStopsAtTheFirstTranscriptDifference(t *testing.T) {
	messages := []projectionMessage{
		{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
		{role: "assistant", text: "", callID: "call_a", command: userBoundaryGit, created: projectionFirstDate},
		{role: "assistant", text: "", callID: "call_b", command: userBoundaryTest, created: projectionSecondDate},
	}
	transcriptMessages := []projectionMessage{
		messages[0],
		messages[1],
		{role: "assistant", text: "", callID: "call_transcript", command: "go vet ./...", created: projectionNextDate},
		messages[2],
	}
	address, home, conversationID := projectionHomeWithTranscript(t, messages, transcriptMessages)
	ctx := context.Background()
	first := "convtool/" + conversationID + "/1/0"
	second := "convtool/" + conversationID + "/1/1"
	third := "convtool/" + conversationID + "/1/2"
	seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
		{path: first, content: projectionGitBase, role: "assistant"},
		{path: second, content: projectionTestBase, role: "assistant"},
		{path: third, content: projectionTestBase, role: "assistant"},
	})
	rows := runProjectionRepair(t, ctx, address, home)
	requireProjectionDate(t, rows, first, projectionFirstDate)
	requireProjectionUndated(t, rows, second)
	requireProjectionUndated(t, rows, third)
}

func TestSourceRepairLeavesAnAmbiguousIdenticalCallUndated(t *testing.T) {
	messages := []projectionMessage{
		{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
		{role: "assistant", text: "", callID: "call_a", command: userBoundaryGit, created: projectionFirstDate},
		{role: "user", text: projectionFollowUp, callID: "", command: "", created: projectionNextDate},
		{role: "assistant", text: "", callID: "call_b", command: userBoundaryGit, created: projectionSecondDate},
	}
	address, home, conversationID := projectionHomeWithTranscript(t, messages, messages[2:])
	ctx := context.Background()
	saved := "convtool/" + conversationID + "/1/0"
	seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
		{path: "conv/" + conversationID + "/0", content: projectionFollowUp, role: "user"},
		{path: saved, content: projectionGitBase, role: "assistant"},
	})
	rows := runProjectionRepair(t, ctx, address, home)
	requireProjectionUndated(t, rows, saved)
}

func TestSourceRepairDatesACallThatHasOneOccurrenceAfterAnOmittedCall(t *testing.T) {
	messages := []projectionMessage{
		{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
		{role: "assistant", text: "", callID: "call_a", command: userBoundaryGit, created: projectionFirstDate},
		{role: "user", text: projectionFollowUp, callID: "", command: "", created: projectionNextDate},
		{role: "assistant", text: "", callID: "call_b", command: userBoundaryTest, created: projectionSecondDate},
	}
	address, home, conversationID := projectionHomeWithTranscript(t, messages, messages[2:])
	ctx := context.Background()
	saved := "convtool/" + conversationID + "/1/0"
	seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
		{path: saved, content: projectionTestBase, role: "assistant"},
	})
	rows := runProjectionRepair(t, ctx, address, home)
	requireProjectionDate(t, rows, saved, projectionSecondDate)
}

func TestSourceRepairIgnoresAStoredUserRowFromAnotherRepresentation(t *testing.T) {
	messages := []projectionMessage{
		{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
		{role: "assistant", text: "", callID: "call_a", command: userBoundaryGit, created: projectionFirstDate},
		{role: "assistant", text: "", callID: "call_b", command: userBoundaryTest, created: projectionSecondDate},
	}
	transcriptMessages := []projectionMessage{
		messages[0],
		messages[1],
		{role: "user", text: projectionFollowUp, callID: "", command: "", created: projectionNextDate},
		messages[2],
	}
	address, home, conversationID := projectionHomeWithTranscript(t, messages, transcriptMessages)
	ctx := context.Background()
	first := "convtool/" + conversationID + "/1/0"
	second := "convtool/" + conversationID + "/3/0"
	seedProjectionRows(t, ctx, address, conversationID, []projectionRow{
		{path: first, content: projectionGitBase, role: "assistant"},
		{path: "conv/" + conversationID + "/2", content: projectionOtherText, role: "user"},
		{path: second, content: projectionTestBase, role: "assistant"},
	})
	rows := runProjectionRepair(t, ctx, address, home)
	requireProjectionDate(t, rows, first, projectionFirstDate)
	requireProjectionDate(t, rows, second, projectionSecondDate)
}

func TestTimestampCorrectionDatesAnOlderRepresentationFromItsOwnProjection(t *testing.T) {
	messages := []projectionMessage{
		{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
		{role: "user", text: projectionFollowUp, callID: "", command: "", created: projectionUserDate},
		{role: "assistant", text: "", callID: "call_a", command: userBoundaryGit, created: projectionFirstDate},
		{role: "user", text: projectionOtherText, callID: "", command: "", created: projectionNextDate},
		{role: "assistant", text: "", callID: "call_b", command: userBoundaryTest, created: projectionSecondDate},
	}
	for _, testCase := range []struct {
		name         string
		contradicted bool
		wantCounts   []string
	}{
		{name: "matching older rows", contradicted: false, wantCounts: []string{"to_known=2", "not_evaluated=1", "written=2"}},
		{name: "contradicted older row", contradicted: true, wantCounts: []string{"to_known=1", "not_evaluated=3", "written=1"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			address, home, conversationID := projectionHome(t, messages)
			ctx := context.Background()
			current := "convtool/" + conversationID + "/1/0"
			older := "convtool/" + conversationID + "/4/0"
			absent := "convtool/" + conversationID + "/4/1"
			contradicted := "convtool/" + conversationID + "/2/0"
			rows := []projectionRow{
				{path: current, content: projectionGitBase, role: "assistant"},
				{path: older, content: projectionTestBase, role: "assistant"},
				{path: absent, content: projectionGitBase, role: "assistant"},
			}
			if testCase.contradicted {
				rows = append(rows, projectionRow{path: contradicted, content: projectionTestBase, role: "assistant"})
			}
			seedProjectionRowsAt(t, ctx, address, conversationID, rows, correctionWrongDate)
			seeded, seededVectors := sharedSnapshot(t, address)
			entries := make([]correctionManifestRow, 0, len(seeded))
			for id, row := range seeded {
				entries = append(entries, correctionManifestEntry(t, id, conversationID, row))
			}
			manifestPath := filepath.Join(home, "manifest.jsonl")
			writeCorrectionManifestEntries(t, manifestPath, entries)
			output, err := runCorrectionCommand(t, ctx, address, "--correction-manifest", manifestPath, "--execute", "--limit", "1", "--progress-file", filepath.Join(home, "progress.json"))
			if err != nil {
				t.Fatalf("failed to run source timestamp repair: %v", err)
			}
			requireCommandOutput(t, output, testCase.wantCounts...)
			requireCommandOutput(t, output, "guard_mismatch=0")
			time.Sleep(repairVisibilityWait)
			after, afterVectors := sharedSnapshot(t, address)
			if len(after) != len(seeded) || len(afterVectors) != len(seededVectors) {
				t.Fatalf("failed to preserve row and vector counts %d, %d, %d, %d", len(after), len(seeded), len(afterVectors), len(seededVectors))
			}
			byPath := make(map[string]sharedStoredRow, len(after))
			for id, row := range after {
				prior := seeded[id]
				if row.relativePath != prior.relativePath || row.content != prior.content || row.vectorKey != prior.vectorKey || row.others != prior.others {
					t.Fatalf("failed to preserve row fields other than timestamps %s: %+v, %+v", id, prior, row)
				}
				byPath[row.relativePath] = row
			}
			requireProjectionDate(t, byPath, current, projectionFirstDate)
			preserved := []string{absent}
			if testCase.contradicted {
				preserved = append(preserved, older, contradicted)
			} else {
				requireProjectionDate(t, byPath, older, projectionSecondDate)
			}
			for _, path := range preserved {
				if row := byPath[path]; row.timestampNull || row.timestampUnix != correctionWrongDate {
					t.Fatalf("failed to find a stored row with the expected date %s: %+v, %d", path, row, correctionWrongDate)
				}
			}
		})
	}
}

func TestSourceRepairDatesAnOlderRunOnlyFromOneNativeOccurrence(t *testing.T) {
	const thirdCommand = "go vet ./..."
	thirdBase := projectionShellName + "\ngo\n" + thirdCommand
	otherBase := projectionShellName + "\nmake\nmake lint"
	for _, testCase := range []struct {
		name    string
		third   string
		rows    []string
		dated   map[int]string
		undated []int
	}{
		{name: "one contiguous occurrence", third: thirdCommand, rows: []string{projectionTestBase, thirdBase}, dated: map[int]string{0: projectionSecondDate, 1: projectionNextDate}, undated: nil},
		{name: "suffix after an earlier difference", third: thirdCommand, rows: []string{otherBase, projectionTestBase, thirdBase}, dated: map[int]string{1: projectionSecondDate, 2: projectionNextDate}, undated: []int{0}},
		{name: "one row", third: thirdCommand, rows: []string{projectionTestBase}, dated: nil, undated: []int{0}},
		{name: "two contiguous occurrences", third: projectionRepeatedCommand, rows: []string{projectionRepeatedBase, projectionRepeatedBase}, dated: nil, undated: []int{0, 1}},
		{name: "wrong order", third: thirdCommand, rows: []string{thirdBase, projectionTestBase}, dated: nil, undated: []int{0, 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			messages := []projectionMessage{
				{role: "user", text: projectionQuestion, callID: "", command: "", created: projectionUserDate},
				{role: "assistant", text: "", callID: "call_a", command: projectionRepeatedCommand, created: projectionFirstDate},
				{role: "assistant", text: "", callID: "call_b", command: userBoundaryTest, created: projectionSecondDate},
				{role: "assistant", text: "", callID: "call_c", command: testCase.third, created: projectionNextDate},
			}
			if testCase.third == projectionRepeatedCommand {
				messages[2].command = projectionRepeatedCommand
			}
			address, home, conversationID := projectionHome(t, messages)
			ctx := context.Background()
			rows := make([]projectionRow, 0, len(testCase.rows))
			for ordinal, content := range testCase.rows {
				rows = append(rows, projectionRow{path: fmt.Sprintf("convtool/%s/9/%d", conversationID, ordinal), content: content, role: "assistant"})
			}
			seedProjectionRows(t, ctx, address, conversationID, rows)
			stored := runProjectionRepair(t, ctx, address, home)
			for ordinal, created := range testCase.dated {
				requireProjectionDate(t, stored, fmt.Sprintf("convtool/%s/9/%d", conversationID, ordinal), created)
			}
			for _, ordinal := range testCase.undated {
				requireProjectionUndated(t, stored, fmt.Sprintf("convtool/%s/9/%d", conversationID, ordinal))
			}
		})
	}
}
