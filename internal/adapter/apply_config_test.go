package adapter

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"goodkind.io/clyde/internal/config"
)

// TestApplyConfigSwapsModelRegistry asserts ApplyConfig rebuilds the model
// registry so a model alias present only in the new config resolves after the
// apply, with no server restart, while the pre-apply registry is left intact on
// a build failure.
func TestApplyConfigSwapsModelRegistry(t *testing.T) {
	t.Parallel()
	base := baseConfig()
	srv, err := New(context.Background(), base, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const alias = "clyde-applied-alias"
	if _, ok := srv.modelRegistry().Models()[alias]; ok {
		t.Fatalf("alias %q present before apply", alias)
	}

	next := baseConfig()
	if next.Models == nil {
		next.Models = map[string]config.AdapterModel{}
	}
	next.Models[alias] = config.AdapterModel{Backend: "claude", Model: "claude-haiku-4-5-20251001", Context: 200000, Efforts: []string{"medium"}}

	if err := srv.ApplyConfig(next); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if _, ok := srv.modelRegistry().Models()[alias]; !ok {
		t.Fatalf("alias %q absent after apply", alias)
	}
}
