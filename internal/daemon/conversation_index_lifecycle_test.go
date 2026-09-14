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
	"goodkind.io/clyde/internal/tokencount"
	"goodkind.io/clyde/internal/transcript"
)

type cachedBoundaryParser struct {
	discoveries *atomic.Int64
	record      conversation.Record
	provider    providerid.Provider
	text        string
}

func (parser *cachedBoundaryParser) Provider() providerid.Provider {
	if parser.provider != providerid.ProviderUnspecified {
		return parser.provider
	}
	return providerid.ProviderCursor
}

func (parser *cachedBoundaryParser) Discover(context.Context, map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	parser.discoveries.Add(1)
	return []conversation.ScanCandidate{{Path: parser.record.ArtifactPath, Stamp: conversation.FileStamp{Size: 1}}}, nil
}

func (parser *cachedBoundaryParser) ScanRecord(string, conversation.FileStamp) (conversation.Record, bool) {
	return parser.record, true
}

func (parser *cachedBoundaryParser) Stream(string, conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		text := parser.text
		if text == "" {
			text = "cached boundary transcript"
		}
		yield(transcript.Message{Role: "user", Text: text}, nil)
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

func TestExportTranscriptLocalAppliesMaxTokens(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(envAnthropic, "")
	t.Setenv(envOpenAI, "")

	var text strings.Builder
	for i := 0; i < 200; i++ {
		text.WriteString("one short transcript line\n")
	}

	parser := &cachedBoundaryParser{
		discoveries: &atomic.Int64{},
		record: conversation.Record{
			ID:           "codex:token-cap",
			Provider:     conversation.ProviderCodex,
			NativeID:     "token-cap",
			ArtifactPath: "/tmp/token-cap.jsonl",
			ArtifactKind: "rollout",
			Model:        "gpt-5.4",
		},
		provider: providerid.ProviderCodex,
		text:     text.String(),
	}
	registry := conversation.NewRegistry()
	registry.Register(parser)
	index := conversation.NewIndex(registry, config.ConversationConfig{})
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	previousFactory := newLocalConversationIndex
	newLocalConversationIndex = func() *conversation.Index {
		return index
	}
	t.Cleanup(func() {
		newLocalConversationIndex = previousFactory
	})

	options := conversation.ExportOptions{
		Format:       conversation.ExportFormatMarkdown,
		HistoryStart: 0,
		LastN:        0,
		MaxLines:     0,
		MaxTokens:    "",
		TokenModel:   "",
		Whitespace:   conversation.WhitespaceDense,
		Content:      conversation.NewContentKindSet(conversation.ContentKindChat),
		Compaction: conversation.CompactionExportOptions{
			IncludeSelector: "0",
			FullHistory:     false,
		},
	}
	uncapped, err := ExportTranscriptLocal(t.Context(), parser.record.ID, options)
	if err != nil {
		t.Fatalf("uncapped export: %v", err)
	}

	options.MaxTokens = "20"
	capped, err := ExportTranscriptLocal(t.Context(), parser.record.ID, options)
	if err != nil {
		t.Fatalf("capped export: %v", err)
	}
	if len(capped) >= len(uncapped) {
		t.Fatalf("capped bytes = %d, uncapped bytes = %d", len(capped), len(uncapped))
	}
	if !strings.HasSuffix(string(uncapped), string(capped)) {
		t.Fatal("capped export is not a suffix of the final rendered body")
	}

	counter := tokencount.LocalCounter(
		tokencount.FamilyGPT,
		parser.record.Model,
		tokencount.Settings{},
	)
	if count := counter.Estimate(string(capped)); count > 20 {
		t.Fatalf("capped token count = %d, want <= 20", count)
	}
}

func TestExportTranscriptLocalRejectsInvalidMaxTokens(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(envAnthropic, "")
	t.Setenv(envOpenAI, "")

	parser := &cachedBoundaryParser{
		discoveries: &atomic.Int64{},
		record: conversation.Record{
			ID:           "codex:invalid-max-tokens",
			Provider:     conversation.ProviderCodex,
			NativeID:     "invalid-max-tokens",
			ArtifactPath: "/tmp/invalid-max-tokens.jsonl",
			ArtifactKind: "rollout",
			Model:        "gpt-5.4",
		},
		provider: providerid.ProviderCodex,
	}
	registry := conversation.NewRegistry()
	registry.Register(parser)
	index := conversation.NewIndex(registry, config.ConversationConfig{})
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	previousFactory := newLocalConversationIndex
	newLocalConversationIndex = func() *conversation.Index {
		return index
	}
	t.Cleanup(func() {
		newLocalConversationIndex = previousFactory
	})

	_, err := ExportTranscriptLocal(t.Context(), parser.record.ID, conversation.ExportOptions{
		Format:     conversation.ExportFormatMarkdown,
		Whitespace: conversation.WhitespaceDense,
		Content:    conversation.NewContentKindSet(conversation.ContentKindChat),
		MaxTokens:  "not-a-token-count",
	})
	if err == nil {
		t.Fatal("expected error for invalid max tokens")
	}
	if !strings.Contains(err.Error(), "parse max tokens") {
		t.Fatalf("error = %q, want parse max tokens", err)
	}
}
