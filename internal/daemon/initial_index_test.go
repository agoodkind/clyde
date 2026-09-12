package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
)

func TestRunInitialConversationIndexBuildsRawIndexWhenIngestionIsDisabled(t *testing.T) {
	configureInitialIndexTest(t, false)

	previousDial := dialInitialSemantic
	calls := 0
	dialInitialSemantic = func(context.Context, string) (initialSemanticClient, error) {
		calls++
		return nil, fmt.Errorf("should not call semantic when disabled")
	}
	t.Cleanup(func() { dialInitialSemantic = previousDial })

	var output bytes.Buffer
	if err := RunInitialConversationIndex(context.Background(), &output, nil); err != nil {
		t.Fatalf("RunInitialConversationIndex: %v", err)
	}
	if calls != 0 {
		t.Fatalf("semantic dial calls = %d, want 0", calls)
	}
	got := output.String()
	if !strings.Contains(got, "Initial indexing: complete with 0 conversations") {
		t.Fatalf("output = %q, missing completion count", got)
	}
	if !strings.Contains(got, "Initial indexing: discovering raw conversations from claude") {
		t.Fatalf("output = %q, missing scoped discovery output", got)
	}
	if !strings.Contains(got, "Initial indexing: finished raw conversation discovery for claude") {
		t.Fatalf("output = %q, missing scoped discovery completion", got)
	}
	if !strings.Contains(got, "Initial indexing: raw conversation discovery elapsed ") {
		t.Fatalf("output = %q, missing heartbeat output", got)
	}
	if _, err := os.Stat(conversation.CachePath()); err != nil {
		t.Fatalf("conversation cache not created: %v", err)
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
		"Initial indexing: starting raw conversation discovery\n",
		"Initial indexing: 0/",
		"Initial indexing: complete with",
		"conversations\n",
		"discovering raw conversations from",
		"finished raw conversation discovery for",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output = %q, missing %q", output.String(), want)
		}
	}
}

func TestInitialIndexRefreshFailureNamesRawDiscovery(t *testing.T) {
	wantErr := errors.New("store unreadable")
	var output bytes.Buffer
	err := refreshInitialConversationIndex(
		t.Context(),
		&output,
		time.Now(),
		func(context.Context) error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("refresh error = %v, want %v", err, wantErr)
	}
	if got := output.String(); !strings.Contains(got, "Initial indexing: raw conversation discovery failed: store unreadable") {
		t.Fatalf("output = %q, missing scoped discovery failure", got)
	}
}

func TestInitialIndexHeartbeatProductionInterval(t *testing.T) {
	if initialIndexHeartbeatInterval != 5*time.Second {
		t.Fatalf("initialIndexHeartbeatInterval = %s, want 5s", initialIndexHeartbeatInterval)
	}
}

func TestInitialIndexHeartbeatStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	output := make(initialIndexHeartbeatOutput, 1)
	done := make(chan struct{})
	go func() {
		writeInitialIndexHeartbeats(ctx, output, time.Now(), time.Millisecond)
		close(done)
	}()

	select {
	case got := <-output:
		if !strings.Contains(got, "Initial indexing: raw conversation discovery elapsed ") {
			t.Fatalf("heartbeat output = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not report progress")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not stop")
	}
}

type initialIndexHeartbeatOutput chan string

func (output initialIndexHeartbeatOutput) Write(payload []byte) (int, error) {
	select {
	case output <- string(payload):
	default:
	}
	return len(payload), nil
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
