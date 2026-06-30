package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
)

const bubbleKeyPrefix = "bubbleId:"

const (
	// BubbleTypeUser identifies a user-authored Cursor composer bubble.
	BubbleTypeUser int = 1
	// BubbleTypeAssistant identifies an assistant-authored Cursor composer bubble.
	BubbleTypeAssistant int = 2
)

// Bubble models the consumed subset of Cursor's undocumented, version-pinned
// on-disk bubble payload. Unconsumed fields are intentionally ignored.
type Bubble struct {
	BubbleID      string
	Type          int
	SchemaVersion int
	Text          string
	Thinking      BubbleThinking
	ToolCall      *BubbleToolCall
}

// BubbleThinking models the consumed assistant reasoning fields in a Cursor
// bubble payload.
type BubbleThinking struct {
	Text string `json:"text"`
}

// BubbleToolCall models the consumed tool call fields in a Cursor bubble
// payload.
type BubbleToolCall struct {
	Name    string `json:"name"`
	RawArgs string `json:"rawArgs"`
	Result  string `json:"result"`
	Status  string `json:"status"`
}

type bubbleWire struct {
	SchemaVersion int             `json:"_v"`
	Type          int             `json:"type"`
	BubbleID      string          `json:"bubbleId"`
	Text          string          `json:"text"`
	Thinking      BubbleThinking  `json:"thinking"`
	ToolCall      *BubbleToolCall `json:"toolFormerData"`
}

// DecodeBubbleJSON decodes one Cursor bubble value. A bubble whose `_v` schema
// version differs from the expected version is still decoded best-effort
// because the consumed fields are stable across versions; callers can detect a
// mismatch via the returned SchemaVersion.
func DecodeBubbleJSON(data []byte) (Bubble, error) {
	var wire bubbleWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return Bubble{}, CursorJSONDecodeError{Description: "bubble", Err: err}
	}
	return Bubble{
		BubbleID:      wire.BubbleID,
		Type:          wire.Type,
		SchemaVersion: wire.SchemaVersion,
		Text:          wire.Text,
		Thinking:      wire.Thinking,
		ToolCall:      wire.ToolCall,
	}, nil
}

// ReadBubble reads and decodes one `bubbleId:<composerId>:<bubbleId>` row from
// a Cursor global database.
func ReadBubble(ctx context.Context, db *sql.DB, composerID string, bubbleID string) (Bubble, bool, error) {
	var emptyBubble Bubble

	value, found, err := ReadKVValue(ctx, db, KVTableCursorDiskKV, bubbleKey(composerID, bubbleID))
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.bubble_read_failed", "concern", concern, "composer_id", composerID, "bubble_id", bubbleID, "err", err)
		return emptyBubble, false, fmt.Errorf("read cursor bubble %q for composer %q: %w", bubbleID, composerID, err)
	}
	if !found {
		return emptyBubble, false, nil
	}

	bubble, err := DecodeBubbleJSON(value)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.bubble_decode_failed", "concern", concern, "composer_id", composerID, "bubble_id", bubbleID, "err", err)
		return emptyBubble, false, fmt.Errorf("decode cursor bubble %q for composer %q: %w", bubbleID, composerID, err)
	}
	return bubble, true, nil
}

// LoadBubblesForComposer loads a composer's referenced bubbles in header order.
// Missing bubble rows are skipped because Cursor may retain stale references.
func LoadBubblesForComposer(ctx context.Context, db *sql.DB, header ComposerHeader) ([]Bubble, error) {
	bubbles := make([]Bubble, 0, len(header.FullConversationHeadersOnly))
	for _, bubbleRef := range header.FullConversationHeadersOnly {
		bubble, found, err := ReadBubble(ctx, db, header.ComposerID, bubbleRef.BubbleID)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.composer_bubbles_load_failed", "concern", concern, "composer_id", header.ComposerID, "bubble_id", bubbleRef.BubbleID, "err", err)
			return nil, fmt.Errorf("load cursor bubbles for composer %q: %w", header.ComposerID, err)
		}
		if !found {
			continue
		}
		bubbles = append(bubbles, bubble)
	}
	return bubbles, nil
}

func bubbleKey(composerID string, bubbleID string) string {
	return bubbleKeyPrefix + composerID + ":" + bubbleID
}
