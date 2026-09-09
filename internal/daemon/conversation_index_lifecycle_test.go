package daemon

import (
	"context"
	"errors"
	"iter"
	"os"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

type lifecycleConversationParser struct {
	started chan struct{}
	stopped chan struct{}
}

func (*lifecycleConversationParser) Provider() providerid.Provider { return providerid.ProviderCursor }

func (parser *lifecycleConversationParser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	close(parser.started)
	<-ctx.Done()
	defer close(parser.stopped)
	return nil, ctx.Err()
}

func (*lifecycleConversationParser) ScanRecord(string, conversation.FileStamp) (conversation.Record, bool) {
	return conversation.Record{}, false
}

func (*lifecycleConversationParser) Stream(string, conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
}

func TestConversationIndexJoinsRefreshDuringLifecycleDrain(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	parser := &lifecycleConversationParser{started: make(chan struct{}), stopped: make(chan struct{})}
	registry := conversation.NewRegistry()
	registry.Register(parser)
	index := conversation.NewIndex(registry, config.ConversationConfig{})
	group := newLifecycleGroup(semanticTestLogger())
	startConversationIndex(t.Context(), semanticTestLogger(), index, group)
	<-parser.started
	group.Quiesce(t.Context(), "reload", livetrack.Budget{Cap: time.Second, IdleGrace: 0})
	assertClosed(t, parser.stopped, "conversation scan survived lifecycle drain")
	if t.Context().Err() != nil {
		t.Fatal("drain must cancel the index without canceling its daemon parent")
	}
	if _, err := os.Stat(conversation.CachePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled scan persisted cache: %v", err)
	}
}
