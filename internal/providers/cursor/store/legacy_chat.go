package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

const legacyChatDataKey = "workbench.panel.aichat.view.aichat.chatdata"

const (
	// LegacyChatRoleUser identifies a user-authored legacy Cursor chat bubble.
	LegacyChatRoleUser string = "user"
	// LegacyChatRoleAssistant identifies an assistant-authored legacy Cursor chat bubble.
	LegacyChatRoleAssistant string = "ai"
)

// LegacyChatData models the consumed subset of Cursor's undocumented,
// version-pinned legacy aichat panel payload.
type LegacyChatData struct {
	Tabs []LegacyChatTab `json:"tabs"`
}

// LegacyChatTab models one consumed legacy aichat panel tab.
type LegacyChatTab struct {
	TabID     string             `json:"tabId"`
	ChatTitle string             `json:"chatTitle"`
	Bubbles   []LegacyChatBubble `json:"bubbles"`
}

// LegacyChatBubble models one consumed legacy aichat panel bubble.
type LegacyChatBubble struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// DecodeLegacyChatDataJSON decodes Cursor's legacy aichat panel payload.
func DecodeLegacyChatDataJSON(data []byte) (LegacyChatData, error) {
	var chatData LegacyChatData
	if err := json.Unmarshal(data, &chatData); err != nil {
		return LegacyChatData{}, CursorJSONDecodeError{Description: "legacy chat data", Err: err}
	}
	return chatData, nil
}

// ReadLegacyChatData reads Cursor's legacy aichat panel data from a workspace
// database ItemTable row.
func ReadLegacyChatData(ctx context.Context, workspaceDB *sql.DB) (LegacyChatData, bool, error) {
	var emptyData LegacyChatData

	value, found, err := ReadKVValue(ctx, workspaceDB, KVTableItemTable, legacyChatDataKey)
	if err != nil {
		return emptyData, false, fmt.Errorf("read cursor legacy chat data: %w", err)
	}
	if !found {
		return emptyData, false, nil
	}

	chatData, err := DecodeLegacyChatDataJSON(value)
	if err != nil {
		return emptyData, false, fmt.Errorf("decode cursor legacy chat data: %w", err)
	}
	return chatData, true, nil
}
