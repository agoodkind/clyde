package claudepath

import (
	"encoding/json"
	"os"
	"testing"
)

func TestWriteModelOverrideSettingsSwapsModelField(t *testing.T) {
	raw := map[string]json.RawMessage{
		"model":         json.RawMessage(`"old-model"`),
		"remoteControl": json.RawMessage(`true`),
	}
	path, err := WriteModelOverrideSettings(raw, "new-model")
	if err != nil {
		t.Fatalf("WriteModelOverrideSettings: %v", err)
	}
	defer func() { _ = os.Remove(path) }()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(contents, &decoded); err != nil {
		t.Fatalf("decode temp file: %v", err)
	}

	var got string
	if err := json.Unmarshal(decoded["model"], &got); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if got != "new-model" {
		t.Fatalf("model = %q, want %q", got, "new-model")
	}
	if _, ok := decoded["remoteControl"]; !ok {
		t.Fatalf("expected remoteControl field preserved in overlay")
	}
}

func TestWriteModelOverrideSettingsAcceptsNilMap(t *testing.T) {
	path, err := WriteModelOverrideSettings(nil, "only-model")
	if err != nil {
		t.Fatalf("WriteModelOverrideSettings: %v", err)
	}
	defer func() { _ = os.Remove(path) }()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(contents, &decoded); err != nil {
		t.Fatalf("decode temp file: %v", err)
	}
	var got string
	if err := json.Unmarshal(decoded["model"], &got); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if got != "only-model" {
		t.Fatalf("model = %q, want %q", got, "only-model")
	}
}
