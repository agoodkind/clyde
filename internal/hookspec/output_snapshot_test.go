package hookspec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteAdditionalContextForCodex(t *testing.T) {
	t.Parallel()

	var builder strings.Builder
	if err := writeAdditionalContext(&builder, ClientCodex, EventSessionStart, "context body"); err != nil {
		t.Fatalf("writeAdditionalContext: %v", err)
	}
	assertAdditionalContextOutput(t, builder.String())
}

func TestWriteAdditionalContextForClaude(t *testing.T) {
	t.Parallel()

	var builder strings.Builder
	if err := writeAdditionalContext(&builder, ClientClaudeCode, EventSessionStart, "context body"); err != nil {
		t.Fatalf("writeAdditionalContext: %v", err)
	}
	assertAdditionalContextOutput(t, builder.String())
}

func assertAdditionalContextOutput(t *testing.T, output string) {
	t.Helper()

	var decoded hookSpecificOutputEnvelope
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("Unmarshal output: %v\n%s", err, output)
	}
	if decoded.HookSpecificOutput.HookEventName != EventSessionStart {
		t.Fatalf("hook event = %q", decoded.HookSpecificOutput.HookEventName)
	}
	if decoded.HookSpecificOutput.AdditionalContext != "context body" {
		t.Fatalf("additional context = %q", decoded.HookSpecificOutput.AdditionalContext)
	}
}

func TestWriteCursorFollowup(t *testing.T) {
	t.Parallel()

	var builder strings.Builder
	if err := writeCursorFollowup(&builder, "follow up"); err != nil {
		t.Fatalf("writeCursorFollowup: %v", err)
	}
	if !strings.Contains(builder.String(), `"followup_message":"follow up"`) {
		t.Fatalf("output = %q", builder.String())
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
