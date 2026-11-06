package parser_test

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	firstTextValue     = "first request"
	firstTextDate      = "2026-05-20T09:01:00.000Z"
	firstTextToolDate  = "2026-05-20T09:02:00.000Z"
	firstTextLateTool  = "2026-05-20T09:20:00.000Z"
	firstTextEarly     = "2026-05-20T08:59:00.000Z"
	firstTextLate      = "2026-05-20T09:03:00.000Z"
	firstTextAfterEdit = "2026-05-20T09:10:00.000Z"
	firstTextCopyDate  = "2026-05-20T09:01:30.000Z"
)

func firstTextTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("cannot parse fixture time: %v", err)
	}
	return parsed
}

// firstTextSourceTime returns the source time of the message with the fixture text.
func firstTextSourceTime(t *testing.T) time.Time {
	t.Helper()
	record := conversation.Record{
		ID: "cursor:" + nativeComposerID, Provider: providerid.ProviderCursor, NativeID: nativeComposerID, Selector: "",
		Lineage: nil, Origin: conversation.OriginUnspecified, Title: "", TitleUncertain: false, WorkspaceRoot: "",
		ArtifactPath: "", ArtifactKind: "", Model: "", CreatedAt: time.Time{}, UpdatedAt: time.Time{}, SizeBytes: 0, Archived: false, LatestRequestID: "",
	}
	source, found, err := parser.New().SourceStoreMessages(t.Context(), record, sourceRootLoadOptions())
	if err != nil || !found {
		t.Fatalf("cannot read source store; found, error: %v, %v", found, err)
	}
	for _, message := range source.Messages {
		if message.Text == firstTextValue {
			return message.Timestamp
		}
	}
	t.Fatalf("source store does not contain the fixture text; messages: %d", len(source.Messages))
	return time.Time{}
}

// TestSourceTextBeforeTheFirstToolUsesComposerBounds checks the date of a text message
// without an earlier dated tool call.
func TestSourceTextBeforeTheFirstToolUsesComposerBounds(t *testing.T) {
	testCases := []struct {
		name      string
		textDate  string
		toolDate  string
		copyDate  string
		afterTool bool
		wantDate  string
	}{
		{name: "the date falls between composer creation and the next dated tool call", textDate: firstTextDate, toolDate: firstTextToolDate, wantDate: firstTextDate},
		{name: "the date falls before composer creation", textDate: firstTextEarly, toolDate: firstTextToolDate},
		{name: "the date falls after the next dated tool call", textDate: firstTextLate, toolDate: firstTextToolDate},
		{name: "the date falls after the composer update time", textDate: firstTextAfterEdit, toolDate: firstTextLateTool},
		{name: "a message stays undated when a stored copy has a different date within the bounds", textDate: firstTextDate, toolDate: firstTextToolDate, copyDate: firstTextCopyDate},
		{name: "a message stays undated when a stored copy has a date after the next dated tool call", textDate: firstTextDate, toolDate: firstTextToolDate, copyDate: firstTextLate},
		{name: "an undated listed bubble stays undated despite a matching dated copy within the bounds", textDate: "", toolDate: firstTextToolDate, copyDate: firstTextDate},
		{name: "the message follows the only dated tool call", textDate: firstTextLate, toolDate: firstTextToolDate, afterTool: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			rootDir := t.TempDir()
			t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
			bubbles := []fixtureBubble{
				userBubble("u1", firstTextValue, testCase.textDate),
				toolBubble("t1", testCase.toolDate, "call_a"),
			}
			if testCase.copyDate != "" {
				copied := userBubble("u1-copy", firstTextValue, testCase.copyDate)
				copied.Unreferenced = true
				bubbles = append(bubbles, copied)
			}
			messages := []fixtureNativeMessage{
				userMessage(firstTextValue),
				assistantMessage(callPart("call_a", commandArgs(t, nativeGitCommand, ""))),
			}
			if testCase.afterTool {
				messages = []fixtureNativeMessage{messages[1], messages[0]}
			}
			writeBoundaryStore(t, rootDir, messages, nil, bubbles)
			got := firstTextSourceTime(t)
			if testCase.wantDate == "" {
				if !got.IsZero() {
					t.Fatalf("the message has a timestamp: %v", got)
				}
				return
			}
			if want := firstTextTime(t, testCase.wantDate); !got.Equal(want) {
				t.Fatalf("message timestamp differs; actual, expected: %v, %v", got, want)
			}
		})
	}
}
