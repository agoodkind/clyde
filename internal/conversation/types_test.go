package conversation

import (
	"encoding/json"
	"testing"
)

func TestRecordJSONUsesProviderLabel(t *testing.T) {
	record := Record{Provider: ProviderClaude}

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
