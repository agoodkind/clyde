package adapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

func TestPreparedResponsesCodexExecutionUsesTrackedEgressContext(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	unblockRequest := make(chan struct{})
	upstream := newLoopbackHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/backend-api/wham/usage" {
			_, _ = writer.Write([]byte(`{}`))
			return
		}
		requestStarted <- struct{}{}
		select {
		case <-request.Context().Done():
		case <-unblockRequest:
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexRoutingSSEBody()))
		}
	}))
	t.Cleanup(func() {
		close(unblockRequest)
	})

	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.BaseURL = upstream.URL + "/backend-api/codex/responses"
	cfg.Codex.WebsocketEnabled = false
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		GetAuth: func(adapterresolver.ProviderID) adapterprovider.AuthLookup {
			return routingTestAuth{}
		},
		Group: group,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	resolved := adapterresolver.ResolvedRequest{
		Provider:  adapterresolver.ProviderCodex,
		Model:     "gpt-5.4",
		RequestID: "req-responses-egress",
		OpenAI: adapteropenai.ChatRequest{Messages: []adapteropenai.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hello"`),
		}}},
	}
	prepared, err := srv.prepareResponsesProvider(context.Background(), resolved)
	if err != nil {
		t.Fatalf("prepareResponsesProvider() error = %v", err)
	}
	executeDone := make(chan error, 1)
	go func() {
		_, executeErr := prepared.Execute(context.Background(), newProviderCollectorWriter(), srv)
		executeDone <- executeErr
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Codex request did not start")
	}
	if active := srv.egressRegistry.Count(); active == 0 {
		t.Error("active egress sessions = 0, want tracked parent and attempt sessions")
	}
	group.Quiesce(context.Background(), "test.responses.force_close", livetrack.Budget{
		Cap:       50 * time.Millisecond,
		IdleGrace: 0,
	})
	select {
	case executeErr := <-executeDone:
		if executeErr == nil {
			t.Error("Execute() error = nil, want cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute() did not return after force-close")
	}
	if active := srv.egressRegistry.Count(); active != 0 {
		t.Fatalf("active egress sessions after Execute = %d, want 0", active)
	}
}
