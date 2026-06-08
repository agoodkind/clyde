package conversation

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecordJSONUsesProviderLabel(t *testing.T) {
	record := emptyRecord()
	record.Provider = ProviderClaude

	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal conversation record: %v", err)
	}

	type providerWire struct {
		Provider string `json:"provider"`
	}
	var wire providerWire
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal provider wire: %v", err)
	}
	if wire.Provider != "claude" {
		t.Fatalf("provider = %q, want claude", wire.Provider)
	}
}

func TestRecordJSONParsesProviderLabel(t *testing.T) {
	var record Record
	if err := json.Unmarshal([]byte(`{"provider":"codex"}`), &record); err != nil {
		t.Fatalf("unmarshal conversation record: %v", err)
	}
	if record.Provider != ProviderCodex {
		t.Fatalf("provider = %v, want %v", record.Provider, ProviderCodex)
	}
}

func TestRecordJSONParsesLegacyProviderEnumValue(t *testing.T) {
	var record Record
	if err := json.Unmarshal([]byte(`{"provider":1}`), &record); err != nil {
		t.Fatalf("unmarshal legacy conversation record: %v", err)
	}
	if record.Provider != ProviderClaude {
		t.Fatalf("provider = %v, want %v", record.Provider, ProviderClaude)
	}
}

func TestRecordJSONRoundTripsForkLineage(t *testing.T) {
	record := testTypeRecord("codex:child", ProviderCodex)
	record.Lineage = &Lineage{
		Kind:              ConversationLineageKindFork,
		ParentProvider:    ProviderClaude,
		ParentNativeID:    "parent-native",
		ParentMessageUUID: "parent-message",
	}

	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal conversation record: %v", err)
	}
	type lineageWire struct {
		ParentProvider string `json:"parent_provider"`
	}
	type recordWire struct {
		Lineage lineageWire `json:"lineage"`
	}
	var wire recordWire
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal conversation record wire: %v", err)
	}
	if wire.Lineage.ParentProvider != "claude" {
		t.Fatalf("parent provider = %q, want claude", wire.Lineage.ParentProvider)
	}

	var decoded Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal conversation record: %v", err)
	}
	if !reflect.DeepEqual(decoded, record) {
		t.Fatalf("decoded record = %#v, want %#v", decoded, record)
	}
}

func TestRecordJSONWithoutLineageDecodesNilLineage(t *testing.T) {
	var record Record
	data := []byte(`{"id":"codex:child","provider":"codex","native_id":"child"}`)
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("unmarshal conversation record: %v", err)
	}
	if record.Lineage != nil {
		t.Fatalf("lineage = %#v, want nil", record.Lineage)
	}
}

func TestRecordJSONNilLineageOmitsLineageKey(t *testing.T) {
	record := testTypeRecord("codex:child", ProviderCodex)

	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal conversation record: %v", err)
	}
	if strings.Contains(string(data), `"lineage"`) {
		t.Fatalf("record JSON included lineage key: %s", data)
	}
}

func testTypeRecord(id string, provider Provider) Record {
	return Record{
		ID:            id,
		Provider:      provider,
		NativeID:      id,
		Lineage:       nil,
		Title:         id,
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/" + id + ".jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}
}
