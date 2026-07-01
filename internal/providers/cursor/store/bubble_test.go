package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestDecodeBubbleJSONDecodesUserAndAssistantFields(t *testing.T) {
	userBubble, err := DecodeBubbleJSON([]byte(`{"_v":3,"type":1,"bubbleId":"bubble-user","text":"hello"}`))
	if err != nil {
		t.Fatalf("DecodeBubbleJSON user returned error: %v", err)
	}
	if userBubble.SchemaVersion != 3 {
		t.Fatalf("SchemaVersion = %d, want 3", userBubble.SchemaVersion)
	}
	if userBubble.Type != BubbleTypeUser {
		t.Fatalf("Type = %d, want BubbleTypeUser", userBubble.Type)
	}
	if userBubble.BubbleID != "bubble-user" {
		t.Fatalf("BubbleID = %q, want bubble-user", userBubble.BubbleID)
	}
	if userBubble.Text != "hello" {
		t.Fatalf("Text = %q, want hello", userBubble.Text)
	}
	if userBubble.ToolCall != nil {
		t.Fatalf("ToolCall = %#v, want nil", userBubble.ToolCall)
	}

	assistantBubble, err := DecodeBubbleJSON([]byte(`{"_v":3,"type":2,"bubbleId":"bubble-assistant","text":"done","thinking":{"text":"working"},"toolFormerData":{"name":"read_file","rawArgs":"{\"path\":\"README.md\"}","result":"contents","status":"success"}}`))
	if err != nil {
		t.Fatalf("DecodeBubbleJSON assistant returned error: %v", err)
	}
	if assistantBubble.Type != BubbleTypeAssistant {
		t.Fatalf("Type = %d, want BubbleTypeAssistant", assistantBubble.Type)
	}
	if assistantBubble.Thinking.Text != "working" {
		t.Fatalf("Thinking.Text = %q, want working", assistantBubble.Thinking.Text)
	}
	if assistantBubble.ToolCall == nil {
		t.Fatal("ToolCall = nil, want tool call")
	}
	if assistantBubble.ToolCall.Name != "read_file" {
		t.Fatalf("ToolCall.Name = %q, want read_file", assistantBubble.ToolCall.Name)
	}
	if assistantBubble.ToolCall.RawArgs != `{"path":"README.md"}` {
		t.Fatalf("ToolCall.RawArgs = %q", assistantBubble.ToolCall.RawArgs)
	}
	if assistantBubble.ToolCall.Result != "contents" {
		t.Fatalf("ToolCall.Result = %q, want contents", assistantBubble.ToolCall.Result)
	}
	if assistantBubble.ToolCall.Status != "success" {
		t.Fatalf("ToolCall.Status = %q, want success", assistantBubble.ToolCall.Status)
	}
}

func TestDecodeBubbleJSONToleratesUnexpectedSchemaVersion(t *testing.T) {
	bubble, err := DecodeBubbleJSON([]byte(`{"_v":4,"type":1,"bubbleId":"bubble-future","text":"hello"}`))
	if err != nil {
		t.Fatalf("DecodeBubbleJSON unexpected version returned error: %v", err)
	}
	if bubble.SchemaVersion != 4 {
		t.Fatalf("SchemaVersion = %d, want 4", bubble.SchemaVersion)
	}
	if bubble.Text != "hello" {
		t.Fatalf("Text = %q, want hello", bubble.Text)
	}
}

func TestDecodeBubbleJSONWrapsInvalidJSON(t *testing.T) {
	_, err := DecodeBubbleJSON([]byte(`{"bubbleId":`))
	if err == nil {
		t.Fatal("DecodeBubbleJSON returned nil error, want decode error")
	}
	var typedErr CursorJSONDecodeError
	if !errors.As(err, &typedErr) {
		t.Fatalf("error = %T, want CursorJSONDecodeError", err)
	}
}

func TestLoadBubblesForComposerReturnsHeaderOrderAndSkipsMissingRefs(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := writable.Exec(
		`UPDATE cursorDiskKV SET value = '{"_v":3,"type":1,"bubbleId":"bubble-1","text":"first"}' WHERE key = 'bubbleId:composer-a:bubble-1'`,
	); err != nil {
		_ = writable.Close()
		t.Fatalf("update bubble-1: %v", err)
	}
	if _, err := writable.Exec(
		`UPDATE cursorDiskKV SET value = '{"_v":3,"type":2,"bubbleId":"bubble-2","text":"second","thinking":{"text":"thought"}}' WHERE key = 'bubbleId:composer-a:bubble-2'`,
	); err != nil {
		_ = writable.Close()
		t.Fatalf("update bubble-2: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	header := ComposerHeader{
		ComposerID: "composer-a",
		FullConversationHeadersOnly: []ComposerBubbleRef{
			{BubbleID: "bubble-2", Type: BubbleTypeAssistant},
			{BubbleID: "missing", Type: BubbleTypeUser},
			{BubbleID: "bubble-1", Type: BubbleTypeUser},
		},
	}
	bubbles, err := LoadBubblesForComposer(context.Background(), readonly, header)
	if err != nil {
		t.Fatalf("LoadBubblesForComposer returned error: %v", err)
	}
	if len(bubbles) != 2 {
		t.Fatalf("bubbles len = %d, want 2", len(bubbles))
	}
	if bubbles[0].BubbleID != "bubble-2" {
		t.Fatalf("bubbles[0].BubbleID = %q, want bubble-2", bubbles[0].BubbleID)
	}
	if bubbles[1].BubbleID != "bubble-1" {
		t.Fatalf("bubbles[1].BubbleID = %q, want bubble-1", bubbles[1].BubbleID)
	}
}
