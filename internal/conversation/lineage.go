package conversation

import (
	"encoding/json"
	"fmt"
	"time"

	"goodkind.io/clyde/internal/providerid"
)

// LineageKind identifies the relationship between two conversations.
type LineageKind string

const (
	// ConversationLineageKindFork means this conversation forked from a parent.
	ConversationLineageKindFork LineageKind = "fork"
	// ConversationLineageKindSpawn means this conversation was spawned by a parent.
	ConversationLineageKindSpawn LineageKind = "spawn"
	// ConversationLineageKindUnknown means the relationship type was not identified.
	ConversationLineageKindUnknown LineageKind = "unknown"
)

// Lineage records a typed parent relationship for one conversation.
type Lineage struct {
	Kind              LineageKind `json:"kind"`
	ParentProvider    Provider    `json:"parent_provider"`
	ParentNativeID    string      `json:"parent_native_id"`
	ParentMessageUUID string      `json:"parent_message_uuid"`
}

// UnmarshalJSON decodes parent_provider through the same enum-backed provider path.
func (lineage *Lineage) UnmarshalJSON(data []byte) error {
	type lineageWire struct {
		Kind              LineageKind     `json:"kind"`
		ParentProvider    json.RawMessage `json:"parent_provider"`
		ParentNativeID    string          `json:"parent_native_id"`
		ParentMessageUUID string          `json:"parent_message_uuid"`
	}
	var wire lineageWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("decode conversation lineage: %w", err)
	}
	parentProvider, err := decodeRecordProvider(wire.ParentProvider)
	if err != nil {
		return fmt.Errorf("decode conversation lineage parent provider: %w", err)
	}
	*lineage = Lineage{
		Kind:              wire.Kind,
		ParentProvider:    parentProvider,
		ParentNativeID:    wire.ParentNativeID,
		ParentMessageUUID: wire.ParentMessageUUID,
	}
	return nil
}

// HasLineage reports whether a conversation record has lineage metadata.
func (r *Record) HasLineage() bool {
	return r.Lineage != nil
}

func emptyRecord() Record {
	return Record{
		ID:            "",
		Provider:      providerid.ProviderUnspecified,
		NativeID:      "",
		Lineage:       nil,
		Title:         "",
		WorkspaceRoot: "",
		ArtifactPath:  "",
		ArtifactKind:  "",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      false,
	}
}
