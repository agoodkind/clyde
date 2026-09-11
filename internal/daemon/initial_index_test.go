package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitialConversationIndexSkipsWhenIngestionIsDisabled(t *testing.T) {
	configureInitialIndexTest(t, false)

	var output bytes.Buffer
	if err := RunInitialConversationIndex(context.Background(), &output, nil); err != nil {
		t.Fatalf("RunInitialConversationIndex: %v", err)
	}
	if got := output.String(); got != "Initial indexing: skipped because ingestion_enabled=false\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestRunInitialConversationIndexReportsProgress(t *testing.T) {
	configureInitialIndexTest(t, true)
	previousDial := dialInitialSemantic
	dialInitialSemantic = func(context.Context, string) (initialSemanticClient, error) {
		return &fakeInitialSemanticClient{
			fakeConversationSemanticClient: &fakeConversationSemanticClient{},
		}, nil
	}
	t.Cleanup(func() { dialInitialSemantic = previousDial })

	var output bytes.Buffer
	if err := RunInitialConversationIndex(context.Background(), &output, func(completed int, total int) {
		_, _ = fmt.Fprintf(&output, "Initial indexing: %d/%d conversations\n", completed, total)
	}); err != nil {
		t.Fatalf("RunInitialConversationIndex: %v", err)
	}
	for _, want := range []string{
		"Initial indexing: starting\n",
		"Initial indexing: 0/",
		"Initial indexing: complete\n",
		"conversations\n",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output = %q, missing %q", output.String(), want)
		}
	}
}

type fakeInitialSemanticClient struct {
	*fakeConversationSemanticClient
}

func (*fakeInitialSemanticClient) Register(context.Context, string) error {
	return nil
}

func (*fakeInitialSemanticClient) Close() error {
	return nil
}

func TestRunInitialConversationIndexTriesSemanticOnce(t *testing.T) {
	configureInitialIndexTest(t, true)
	var attempts int
	previousDial := dialInitialSemantic
	dialInitialSemantic = func(context.Context, string) (initialSemanticClient, error) {
		attempts++
		return nil, fmt.Errorf("semantic engine unavailable")
	}
	t.Cleanup(func() { dialInitialSemantic = previousDial })

	var output bytes.Buffer
	if err := RunInitialConversationIndex(context.Background(), &output, nil); err != nil {
		t.Fatalf("RunInitialConversationIndex: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("semantic dial attempts = %d, want 1", attempts)
	}
	if got := strings.Count(output.String(), "semantic service is unavailable"); got != 1 {
		t.Fatalf("semantic unavailable messages = %d, want 1: %q", got, output.String())
	}
}

func configureInitialIndexTest(t *testing.T, ingestionEnabled bool) {
	t.Helper()
	home := t.TempDir()
	configHome := t.TempDir()
	cacheHome := t.TempDir()
	stateHome := t.TempDir()
	runtimeHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)
	configDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[conversation.semantic]\ningestion_enabled = false\nsearch_enabled = false\n"
	if ingestionEnabled {
		body = "[conversation.semantic]\ningestion_enabled = true\nsearch_enabled = false\n"
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
