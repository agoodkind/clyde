package model

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/slogger"
)

func TestModelResolveLogBindsSingleResolveConcern(t *testing.T) {
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	modelResolveLog.Logger().Warn("adapter.registry.resolve_test")

	body := strings.TrimSpace(buffer.String())
	if count := strings.Count(body, `"concern"`); count != 1 {
		t.Fatalf("concern attribute count = %d, want 1; event=%s", count, body)
	}
	var event struct {
		Concern string `json:"concern"`
	}
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatalf("unmarshal log event: %v; event=%s", err, body)
	}
	if event.Concern != slogger.ConcernAdapterModelsResolve {
		t.Fatalf("concern = %q, want %q", event.Concern, slogger.ConcernAdapterModelsResolve)
	}
}
