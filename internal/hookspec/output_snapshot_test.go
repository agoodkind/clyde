package hookspec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteAdditionalContextForCodex(t *testing.T) {
	t.Parallel()

	note := buildReorientAfterCompactNote("/tmp/session.jsonl")
	var builder strings.Builder
	if err := writeAdditionalContext(&builder, ClientCodex, EventSessionStart, note); err != nil {
		t.Fatalf("writeAdditionalContext: %v", err)
	}
	assertAdditionalContextOutput(t, builder.String(), note)
}

func TestWriteAdditionalContextForClaude(t *testing.T) {
	t.Parallel()

	note := buildReorientAfterCompactNote("/tmp/session.jsonl")
	var builder strings.Builder
	if err := writeAdditionalContext(&builder, ClientClaudeCode, EventSessionStart, note); err != nil {
		t.Fatalf("writeAdditionalContext: %v", err)
	}
	assertAdditionalContextOutput(t, builder.String(), note)
}

func assertAdditionalContextOutput(t *testing.T, output string, want string) {
	t.Helper()

	var decoded hookSpecificOutputEnvelope
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("Unmarshal output: %v\n%s", err, output)
	}
	if decoded.HookSpecificOutput.HookEventName != EventSessionStart {
		t.Fatalf("hook event = %q", decoded.HookSpecificOutput.HookEventName)
	}
	if decoded.HookSpecificOutput.AdditionalContext != want {
		t.Fatalf("additional context = %q", decoded.HookSpecificOutput.AdditionalContext)
	}
}

func TestWriteCursorFollowup(t *testing.T) {
	t.Parallel()

	note := buildReorientAfterCompactNote("/tmp/cursor.jsonl")
	var builder strings.Builder
	if err := writeCursorFollowup(&builder, note); err != nil {
		t.Fatalf("writeCursorFollowup: %v", err)
	}
	var decoded cursorFollowupOutput
	if err := json.Unmarshal([]byte(builder.String()), &decoded); err != nil {
		t.Fatalf("Unmarshal output: %v\n%s", err, builder.String())
	}
	assertReorientAfterCompactNote(t, decoded.FollowupMessage, "/tmp/cursor.jsonl")
}

func TestReorientAfterCompactNoteIncludesPagingInstructions(t *testing.T) {
	t.Parallel()

	note := buildReorientAfterCompactNote("/tmp/session.jsonl")

	assertReorientAfterCompactNote(t, note, "/tmp/session.jsonl")
}

func assertReorientAfterCompactNote(t *testing.T, body string, selector string) {
	t.Helper()

	if len(body) >= 10_000 {
		t.Fatalf("note length = %d, want under hook output limit", len(body))
	}
	lowerBody := strings.ToLower(body)
	requiredParts := []string{
		"conversation was just compacted",
		"before continuing",
		"clyde reorient tool",
		"conversation set to",
		"returned cursor",
		"remaining is zero",
	}
	for _, requiredPart := range requiredParts {
		if !strings.Contains(lowerBody, requiredPart) {
			t.Fatalf("note missing %q:\n%s", requiredPart, body)
		}
	}
	if !strings.Contains(body, selector) {
		t.Fatalf("note missing selector %q:\n%s", selector, body)
	}
}

func TestWriteAdditionalContextRejectsUnsupportedClient(t *testing.T) {
	t.Parallel()

	var builder strings.Builder
	err := writeAdditionalContext(&builder, ClientCursor, EventSessionStart, "context body")
	if err == nil {
		t.Fatal("writeAdditionalContext returned nil error")
	}
	if !strings.Contains(err.Error(), "does not support additional context") {
		t.Fatalf("error = %v", err)
	}
}

func TestFileSnapshotStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := NewFileSnapshotStore(t.TempDir())
	key := ReorientSnapshotKey{
		TranscriptPath: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	}
	if err := store.Save(context.Background(), key, "saved body"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Consume(context.Background(), key)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !ok || got != "saved body" {
		t.Fatalf("Consume = (%q, %t), want (saved body, true)", got, ok)
	}
	got, ok, err = store.Consume(context.Background(), key)
	if err != nil {
		t.Fatalf("second Consume: %v", err)
	}
	if ok || got != "" {
		t.Fatalf("second Consume = (%q, %t), want missing snapshot", got, ok)
	}
}

func TestSnapshotKeyNormalizesTranscriptPathAndConversationID(t *testing.T) {
	t.Parallel()

	fromTranscript := normalizeSnapshotKey(ReorientSnapshotKey{
		TranscriptPath: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	})
	fromConversation := normalizeSnapshotKey(ReorientSnapshotKey{
		ConversationID: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	})
	if fromTranscript != fromConversation {
		t.Fatalf("normalized keys differ: %#v != %#v", fromTranscript, fromConversation)
	}
}
