package parser_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	chronologyOpening = "Start the retention review."
	chronologyReply   = "The retention review found three expired snapshots."
	chronologyFollow  = "Delete the expired snapshots now."
)

// chronologyCopy creates an unreferenced assistant reply bubble with a server bubble ID.
func chronologyCopy(bubbleID string, created string) fixtureBubble {
	copied := assistantBubble(bubbleID, chronologyReply, created)
	copied.ServerBubbleID = "71f3024c-3b7b-5727-8af4-51838c6c15dd"
	copied.Unreferenced = true
	return copied
}

// TestTranscriptMessagesTakeTheOnlyCandidateDateInsideTheOriginalToolInterval checks
// that the transcript reply uses the only candidate date between original tool anchors.
// It also checks that both user messages keep unknown timestamps.
func TestTranscriptMessagesTakeTheOnlyCandidateDateInsideTheOriginalToolInterval(t *testing.T) {
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, chronologyOpening),
		toolLine(t, commandArgs(t, nativeGitCommand, "")),
		assistantTextLine(t, chronologyReply),
		userLine(t, chronologyFollow),
		toolLine(t, commandArgs(t, nativeTestCommand, "")),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(chronologyOpening),
		assistantMessage(callPart("call_early", commandArgs(t, nativeGitCommand, ""))),
		{Role: "assistant", Content: []fixturePart{{Type: "text", Text: chronologyReply, ToolCallID: "", ToolName: "", Args: nil}}},
		userMessage(chronologyFollow),
		assistantMessage(callPart("call_late", commandArgs(t, nativeTestCommand, ""))),
	}, nil, []fixtureBubble{
		userBubble("u1", chronologyOpening, "2026-03-29T22:30:00.000Z"),
		toolBubble("t1", "2026-03-29T22:37:46.036Z", "call_early"),
		assistantBubble("a1", chronologyReply, "2026-03-29T22:42:45.253Z"),
		chronologyCopy("a2", "2026-03-29T23:26:06.115Z"),
		chronologyCopy("a3", "2026-03-30T00:15:06.453Z"),
		chronologyCopy("a4", "2026-03-30T00:22:03.704Z"),
		chronologyCopy("a5", "2026-03-30T05:39:38.181Z"),
		userBubble("u2", chronologyFollow, "2026-03-30T05:39:38.181Z"),
		toolBubble("t2", "2026-03-29T22:45:59.728Z", "call_late"),
	})
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("cannot collect transcript messages: %v", err)
	}
	stamps := map[string]time.Time{}
	for _, message := range messages {
		if message.Text != "" {
			stamps[message.Text] = message.Timestamp
		}
	}
	if got := stamps[chronologyReply]; got.Unix() != 1774824165 {
		t.Fatalf("assistant reply timestamp differs from 1774824165; actual timestamp: %v", got)
	}
	if got := stamps[chronologyFollow]; !got.IsZero() {
		t.Fatalf("follow-up user message has a known timestamp; actual timestamp: %v", got)
	}
	if got := stamps[chronologyOpening]; !got.IsZero() {
		t.Fatalf("opening user message has a known timestamp; actual timestamp: %v", got)
	}
}

const (
	chronologyRequestID        = "request-shared"
	chronologyArchiveReference = 0x6a
)

type chronologyCursorOptions struct {
	RequestID string `json:"requestId"`
}

type chronologyProviderOptions struct {
	Cursor chronologyCursorOptions `json:"cursor"`
}

type chronologyNativeMessage struct {
	Role            string                    `json:"role"`
	Content         []fixturePart             `json:"content,omitempty"`
	ProviderOptions chronologyProviderOptions `json:"providerOptions"`
}

func chronologyBlobReference(t *testing.T, field byte, message any) (kvRow, []byte) {
	t.Helper()
	data := mustMarshal(t)(json.Marshal(message))
	digest := sha256.Sum256(data)
	reference := append([]byte{field, byte(len(digest))}, digest[:]...)
	return kvRow{key: "agentKv:blob:" + hex.EncodeToString(digest[:]), text: "", blob: data}, reference
}

func writeLinkedSequenceStore(t *testing.T, rootDir string, userContent []fixturePart) {
	t.Helper()
	sharedOptions := chronologyProviderOptions{Cursor: chronologyCursorOptions{RequestID: chronologyRequestID}}
	emptyOptions := chronologyProviderOptions{Cursor: chronologyCursorOptions{RequestID: ""}}
	shared := chronologyNativeMessage{Role: "user", Content: userContent, ProviderOptions: sharedOptions}
	early := chronologyNativeMessage{Role: "assistant", Content: []fixturePart{callPart("call_early", commandArgs(t, nativeGitCommand, ""))}, ProviderOptions: emptyOptions}
	reply := chronologyNativeMessage{Role: "assistant", Content: []fixturePart{{Type: "text", Text: chronologyReply, ToolCallID: "", ToolName: "", Args: nil}}, ProviderOptions: emptyOptions}
	late := chronologyNativeMessage{Role: "assistant", Content: []fixturePart{callPart("call_late", commandArgs(t, nativeTestCommand, ""))}, ProviderOptions: emptyOptions}
	rows := make([]kvRow, 0)
	archive := make([]byte, 0)
	for _, message := range []chronologyNativeMessage{early, shared} {
		row, reference := chronologyBlobReference(t, fixtureFieldReference, message)
		rows = append(rows, row)
		archive = append(archive, reference...)
	}
	archiveDigest := sha256.Sum256(archive)
	rows = append(rows, kvRow{key: "agentKv:blob:" + hex.EncodeToString(archiveDigest[:]), text: "", blob: archive})
	state := append([]byte{chronologyArchiveReference, byte(len(archiveDigest))}, archiveDigest[:]...)
	_, sharedReference := chronologyBlobReference(t, fixtureFieldReference, shared)
	state = append(state, sharedReference...)
	for _, message := range []chronologyNativeMessage{reply, late} {
		row, reference := chronologyBlobReference(t, fixtureFieldReference, message)
		rows = append(rows, row)
		state = append(state, reference...)
	}
	bubbles := []fixtureBubble{
		toolBubble("t1", "2026-03-29T22:37:46.036Z", "call_early"),
		assistantBubble("a1", chronologyReply, "2026-03-29T22:42:45.253Z"),
		toolBubble("t2", "2026-03-29T22:45:59.728Z", "call_late"),
	}
	composer := fixtureComposer{
		ComposerID: nativeComposerID, Name: "Fixture", CreatedAt: fixtureComposerCreated, LastUpdatedAt: fixtureComposerUpdated,
		ConversationState: fixtureStatePrefix + base64.StdEncoding.EncodeToString(state), Headers: make([]fixtureHeaderRef, 0, len(bubbles)),
	}
	for _, bubble := range bubbles {
		composer.Headers = append(composer.Headers, fixtureHeaderRef{BubbleID: bubble.BubbleID, Type: bubble.Type})
		rows = append(rows, kvRow{key: "bubbleId:" + nativeComposerID + ":" + bubble.BubbleID, text: string(mustMarshal(t)(json.Marshal(bubble))), blob: nil})
	}
	rows = append(rows, kvRow{key: "composerData:" + nativeComposerID, text: string(mustMarshal(t)(json.Marshal(composer))), blob: nil})
	writeKVStore(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), rows)
}

func TestTranscriptReplyCrossesSequencesOnlyThroughAUserWithContent(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		userContent []fixturePart
		dated       bool
	}{
		{name: "matching content", userContent: []fixturePart{{Type: "text", Text: chronologyOpening, ToolCallID: "", ToolName: "", Args: nil}}, dated: true},
		{name: "empty content", userContent: nil, dated: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rootDir, transcriptPath := boundaryRoots(t, []string{
				toolLine(t, commandArgs(t, nativeGitCommand, "")),
				assistantTextLine(t, chronologyReply),
				toolLine(t, commandArgs(t, nativeTestCommand, "")),
			})
			writeLinkedSequenceStore(t, rootDir, testCase.userContent)
			messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
			if err != nil {
				t.Fatalf("transcript message collection failed: %v", err)
			}
			for _, message := range messages {
				if message.Text == chronologyReply && message.Timestamp.IsZero() == testCase.dated {
					t.Fatalf("reply timestamp does not match the expected dated state: %v, %v", message.Timestamp, testCase.dated)
				}
			}
		})
	}
}
