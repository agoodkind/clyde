package daemon

import (
	"context"
	"errors"
	"iter"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/hookspec"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

type cachedBoundaryParser struct {
	discoveries *atomic.Int64
	record      conversation.Record
}

func (*cachedBoundaryParser) Provider() providerid.Provider { return providerid.ProviderCursor }

func (parser *cachedBoundaryParser) Discover(context.Context, map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	parser.discoveries.Add(1)
	return []conversation.ScanCandidate{{Path: parser.record.ArtifactPath, Stamp: conversation.FileStamp{Size: 1}}}, nil
}

func (parser *cachedBoundaryParser) ScanRecord(string, conversation.FileStamp) (conversation.Record, bool) {
	return parser.record, true
}

func (*cachedBoundaryParser) Stream(string, conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		yield(transcript.Message{Role: "user", Text: "cached boundary transcript"}, nil)
	}
}

func TestCachedRequestHookAndExportTranscriptLocalSkipProviderDiscovery(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	discoveries := &atomic.Int64{}
	requestID := "0e3f0000-0000-4000-8000-000000000000"
	parser := &cachedBoundaryParser{
		discoveries: discoveries,
		record: conversation.Record{
			ID:              "cursor:cached-boundary",
			Provider:        conversation.ProviderCursor,
			NativeID:        "cached-boundary",
			ArtifactPath:    "/tmp/cached-boundary.jsonl",
			ArtifactKind:    "composer",
			LatestRequestID: requestID,
		},
	}
	registry := conversation.NewRegistry()
	registry.Register(parser)
	index := conversation.NewIndex(registry, config.ConversationConfig{})
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	discoveries.Store(0)

	server := &controlServer{index: index}
	runner := hookspec.Runner{
		Registry:      hookspec.NewRegistry(),
		Input:         strings.NewReader(`{"hook_event_name":"PreCompact","conversation_id":"` + requestID + `","cwd":"/tmp/project","session_id":"session-1"}`),
		Output:        &strings.Builder{},
		SnapshotStore: hookspec.NewFileSnapshotStore(t.TempDir()),
		Reorient: func(ctx context.Context, conversationID, workspace, cursor string, maxLines, pageBytes int, includeToolOutputs, syntheticPreCompact bool) (conversation.ReorientPage, error) {
			response, err := server.ReorientConversation(ctx, &clydev1.ReorientConversationRequest{
				ConversationId:      conversationID,
				Workspace:           workspace,
				Cursor:              cursor,
				MaxLines:            int64(maxLines),
				PageBytes:           int64(pageBytes),
				IncludeToolOutputs:  includeToolOutputs,
				SyntheticPrecompact: syntheticPreCompact,
			})
			if err != nil {
				return conversation.ReorientPage{}, err
			}
			return reorientPageFromProto(response), nil
		},
	}
	if err := runner.Run(t.Context(), hookspec.HookIDReorientBeforeCompact); err != nil {
		t.Fatal(err)
	}

	previousFactory := newLocalConversationIndex
	newLocalConversationIndex = func() *conversation.Index { return index }
	t.Cleanup(func() { newLocalConversationIndex = previousFactory })
	body, err := ExportTranscriptLocal(t.Context(), requestID, conversation.ExportOptions{
		Format:     conversation.ExportFormatMarkdown,
		Whitespace: conversation.WhitespacePreserve,
		Content:    conversation.NewContentKindSet(conversation.ContentKindChat),
	})
	if err != nil || !strings.Contains(string(body), "cached boundary transcript") {
		t.Fatalf("export = %q, %v", body, err)
	}
	if discoveries.Load() != 0 {
		t.Fatalf("provider discoveries = %d, want 0", discoveries.Load())
	}
}

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
