package config

import (
	"context"
	"log/slog"
	"testing"
)

// capturingHandler records every emitted slog record so a test can assert the
// removed-key warnings without touching the filesystem.
type capturingHandler struct {
	records *[]slog.Record
}

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, record slog.Record) error {
	// slog may reuse or mutate the record after Handle returns, so retain a
	// clone for later inspection (slog.Handler contract).
	*h.records = append(*h.records, record.Clone())
	return nil
}

func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h capturingHandler) WithGroup(string) slog.Handler { return h }

// TestWarnRemovedLoggingConfigWarnsOnRemovedAdapterWireCapture pins that a stale
// per-provider [adapter.<provider>.wire_capture] block, which go-toml/v2 would
// otherwise ignore silently, emits a removed-key warning for each provider.
func TestWarnRemovedLoggingConfigWarnsOnRemovedAdapterWireCapture(t *testing.T) {
	const tomlData = "[adapter.anthropic.wire_capture]\nmode = \"full\"\n" +
		"[adapter.codex.wire_capture]\nmode = \"full\"\n"

	var records []slog.Record
	logger := slog.New(capturingHandler{records: &records})
	warnRemovedLoggingConfig([]byte(tomlData), "test.toml", logger)

	gotKeys := map[string]bool{}
	for _, record := range records {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "removed_key" {
				gotKeys[attr.Value.String()] = true
			}
			return true
		})
	}

	wantKeys := []string{"adapter.anthropic.wire_capture", "adapter.codex.wire_capture"}
	for _, want := range wantKeys {
		if !gotKeys[want] {
			t.Fatalf("missing removed-key warning for %q; got %v", want, gotKeys)
		}
	}
}
