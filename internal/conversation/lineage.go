package conversation

import (
	"context"
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

// ParentConversationID returns the derived conversation id of a record's lineage
// parent. The second result is false when the record carries no lineage or its
// parent native id is empty, so callers can distinguish "this record has no
// resolvable parent" from a real derived id. It uses the same [DerivedID]
// derivation as every other record id, so the result matches the parent's own id
// in the index.
func ParentConversationID(record Record) (string, bool) {
	if record.Lineage == nil {
		return "", false
	}
	if record.Lineage.ParentNativeID == "" {
		return "", false
	}
	return DerivedID(record.Lineage.ParentProvider, record.Lineage.ParentNativeID, ""), true
}

// ResolveForkParent resolves the parent record of a forked conversation from the
// index's in-memory snapshot. It returns (parent, true, nil) only when record
// carries fork lineage whose derived parent id is present in the index. Spawn,
// unknown, or absent lineage, an empty parent id, a nil index, or a parent that
// is not in the index all yield (emptyRecord(), false, nil); a parent missing
// from the index is not an error. The lookup is a snapshot read with no refresh,
// so it stays cheap on the daemon's read path.
func ResolveForkParent(ctx context.Context, idx *Index, record Record) (Record, bool, error) {
	if idx == nil {
		return emptyRecord(), false, nil
	}
	if record.Lineage == nil || record.Lineage.Kind != ConversationLineageKindFork {
		return emptyRecord(), false, nil
	}
	parentID, ok := ParentConversationID(record)
	if !ok {
		return emptyRecord(), false, nil
	}
	parent, found := idx.RecordByID(parentID)
	if !found {
		return emptyRecord(), false, nil
	}
	return parent, true, nil
}

func emptyRecord() Record {
	return Record{
		ID:              "",
		Provider:        providerid.ProviderUnspecified,
		NativeID:        "",
		Lineage:         nil,
		Origin:          OriginUnspecified,
		Title:           "",
		TitleUncertain:  false,
		WorkspaceRoot:   "",
		ArtifactPath:    "",
		ArtifactKind:    "",
		Model:           "",
		CreatedAt:       time.Time{},
		UpdatedAt:       time.Time{},
		SizeBytes:       0,
		Archived:        false,
		LatestRequestID: "",
	}
}
