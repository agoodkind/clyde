package search

import (
	"context"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

// sentinelClient is a Client whose identity a test can assert on.
type sentinelClient struct {
	id string
}

func (sentinelClient) Complete(context.Context, string) (string, error) {
	return "", nil
}

// TestRegisterBackendRoutesToFactory asserts a registered backend factory
// receives the requested model and its client is returned by newClientForModel.
func TestRegisterBackendRoutesToFactory(t *testing.T) {
	gotModel := ""
	RegisterBackend("test-fake-backend", func(model string) Client {
		gotModel = model
		return sentinelClient{id: "fake"}
	})

	var local config.SearchLocal
	client := newClientForModel(config.SearchConfig{Backend: "test-fake-backend", Local: local}, "my-model")
	sentinel, ok := client.(sentinelClient)
	if !ok {
		t.Fatalf("client type = %T, want sentinelClient", client)
	}
	if sentinel.id != "fake" {
		t.Fatalf("sentinel id = %q, want fake", sentinel.id)
	}
	if gotModel != "my-model" {
		t.Fatalf("factory model = %q, want my-model", gotModel)
	}
}

// TestNewClientForModelLocalBackendStaysInline asserts the built-in local
// backend is constructed directly rather than through the registry.
func TestNewClientForModelLocalBackendStaysInline(t *testing.T) {
	var local config.SearchLocal
	client := newClientForModel(config.SearchConfig{Backend: "local", Local: local}, "m")
	if _, ok := client.(*localClient); !ok {
		t.Fatalf("client type = %T, want *localClient", client)
	}
}

// TestNewClientForModelUnregisteredBackendErrors asserts an unknown backend
// yields a client that reports a clear registration error rather than panicking.
func TestNewClientForModelUnregisteredBackendErrors(t *testing.T) {
	var local config.SearchLocal
	client := newClientForModel(config.SearchConfig{Backend: "definitely-not-registered", Local: local}, "m")
	_, err := client.Complete(context.Background(), "prompt")
	if err == nil {
		t.Fatalf("expected an error for an unregistered backend")
	}
	if !strings.Contains(err.Error(), "definitely-not-registered") {
		t.Fatalf("error = %q, want it to name the backend", err.Error())
	}
}

// TestNewClientForModelEmptyBackendDefaultsToClaude asserts an empty backend
// resolves to the default "claude" name. The Claude backend lives in a separate
// package not imported by this test binary, so the default surfaces as a
// registration error that names "claude", which proves the defaulting.
func TestNewClientForModelEmptyBackendDefaultsToClaude(t *testing.T) {
	var local config.SearchLocal
	client := newClientForModel(config.SearchConfig{Backend: "", Local: local}, "m")
	_, err := client.Complete(context.Background(), "prompt")
	if err == nil {
		t.Fatalf("expected an error because the claude backend is not registered in this test binary")
	}
	if !strings.Contains(err.Error(), defaultBackend) {
		t.Fatalf("error = %q, want it to name the default backend %q", err.Error(), defaultBackend)
	}
}
