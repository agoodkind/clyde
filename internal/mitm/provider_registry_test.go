package mitm

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestProviderIDLogValueUsesStableLabel(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))

	logger.Info("provider-label", "provider", ProviderIDCodex)

	if !bytes.Contains(buffer.Bytes(), []byte(`"provider":"codex"`)) {
		t.Fatalf("provider log = %s, want stable provider label", buffer.String())
	}
	if bytes.Contains(buffer.Bytes(), []byte(`\u0002`)) {
		t.Fatalf("provider log = %s, want no numeric enum rune", buffer.String())
	}
}
