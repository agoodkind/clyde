package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestDecodeLegacyChatDataJSONDecodesTabsAndStringRoles(t *testing.T) {
	data, err := DecodeLegacyChatDataJSON([]byte(`{"tabs":[{"tabId":"tab-a","chatTitle":"Legacy chat","bubbles":[{"type":"user","text":"hello"},{"type":"ai","text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeLegacyChatDataJSON returned error: %v", err)
	}
	if len(data.Tabs) != 1 {
		t.Fatalf("Tabs len = %d, want 1", len(data.Tabs))
	}
	tab := data.Tabs[0]
	if tab.TabID != "tab-a" {
		t.Fatalf("TabID = %q, want tab-a", tab.TabID)
	}
	if tab.ChatTitle != "Legacy chat" {
		t.Fatalf("ChatTitle = %q, want Legacy chat", tab.ChatTitle)
	}
	if len(tab.Bubbles) != 2 {
		t.Fatalf("Bubbles len = %d, want 2", len(tab.Bubbles))
	}
	if tab.Bubbles[0].Type != LegacyChatRoleUser {
		t.Fatalf("first bubble type = %q, want user", tab.Bubbles[0].Type)
	}
	if tab.Bubbles[1].Type != LegacyChatRoleAssistant {
		t.Fatalf("second bubble type = %q, want ai", tab.Bubbles[1].Type)
	}
	if tab.Bubbles[1].Text != "hi" {
		t.Fatalf("second bubble text = %q, want hi", tab.Bubbles[1].Text)
	}
}

func TestDecodeLegacyChatDataJSONWrapsInvalidJSON(t *testing.T) {
	_, err := DecodeLegacyChatDataJSON([]byte(`{"tabs":`))
	if err == nil {
		t.Fatal("DecodeLegacyChatDataJSON returned nil error, want decode error")
	}
	var typedErr CursorJSONDecodeError
	if !errors.As(err, &typedErr) {
		t.Fatalf("error = %T, want CursorJSONDecodeError", err)
	}
}

func TestReadLegacyChatDataReadsWorkspaceItemTableKey(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := writable.Exec(
		`INSERT INTO ItemTable(key, value) VALUES ('workbench.panel.aichat.view.aichat.chatdata', '{"tabs":[{"tabId":"tab-a","chatTitle":"Legacy chat","bubbles":[{"type":"user","text":"question"}]}]}')`,
	); err != nil {
		_ = writable.Close()
		t.Fatalf("insert legacy chat data: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	data, found, err := ReadLegacyChatData(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadLegacyChatData returned error: %v", err)
	}
	if !found {
		t.Fatal("ReadLegacyChatData found = false, want true")
	}
	if data.Tabs[0].Bubbles[0].Text != "question" {
		t.Fatalf("legacy bubble text = %q, want question", data.Tabs[0].Bubbles[0].Text)
	}
}

func TestReadLegacyChatDataReportsMissingKey(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	data, found, err := ReadLegacyChatData(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadLegacyChatData returned error: %v", err)
	}
	if found {
		t.Fatal("ReadLegacyChatData found = true, want false")
	}
	if len(data.Tabs) != 0 {
		t.Fatalf("missing data Tabs len = %d, want 0", len(data.Tabs))
	}
}
