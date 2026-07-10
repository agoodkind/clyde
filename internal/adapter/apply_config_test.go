package adapter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
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
	next.Models[alias] = config.AdapterModelDeclaration{
		Provider:  config.AdapterModelProviderAnthropic,
		WireModel: "claude-haiku-4-5-20251001",
		Profile:   "haiku",
	}

	if err := srv.ApplyConfig(next); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if _, ok := srv.modelRegistry().Models()[alias]; !ok {
		t.Fatalf("alias %q absent after apply", alias)
	}
}

func TestApplyConfigDoesNotRetargetResolvedPassthroughRequest(t *testing.T) {
	t.Parallel()
	var oldRequests atomic.Int32
	oldUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		oldRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"old"}}]}`))
	}))
	t.Cleanup(oldUpstream.Close)
	var newRequests atomic.Int32
	newUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		newRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"new"}}]}`))
	}))
	t.Cleanup(newUpstream.Close)

	base := passthroughSnapshotConfig(oldUpstream.URL)
	srv, err := New(context.Background(), base, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resolved, err := adapterresolver.Resolve(
		adapterresolver.IngressOpenAI,
		adaptercursor.Request{OpenAI: adapteropenai.ChatRequest{Model: "snapshot-alias"}},
		adapterresolver.NewModelRegistryAdapter(srv.modelRegistry()),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := srv.ApplyConfig(passthroughSnapshotConfig(newUpstream.URL)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	srv.forwardPassthroughOverride(
		recorder,
		request,
		&resolved,
		[]byte(`{"model":"snapshot-alias","messages":[{"role":"user","content":"hello"}]}`),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if oldRequests.Load() != 1 || newRequests.Load() != 0 {
		t.Fatalf("request counts old=%d new=%d", oldRequests.Load(), newRequests.Load())
	}
}

func passthroughSnapshotConfig(baseURL string) config.AdapterConfig {
	cfg := baseConfig()
	tools := true
	vision := false
	cfg.ModelProfiles["snapshot"] = config.AdapterModelProfile{
		Contexts:        []config.AdapterModelProfileContext{{Name: "standard", Tokens: 8000}},
		MaxOutputTokens: 1000,
		SupportsTools:   &tools,
		SupportsVision:  &vision,
	}
	cfg.PassthroughOverrides = map[string]config.AdapterPassthroughOverride{
		"snapshot": {BaseURL: baseURL, Model: "wire-snapshot"},
	}
	cfg.Models["snapshot-alias"] = config.AdapterModelDeclaration{
		Provider:            config.AdapterModelProviderPassthroughOverride,
		WireModel:           "wire-snapshot",
		Profile:             "snapshot",
		PassthroughOverride: "snapshot",
	}
	return cfg
}
