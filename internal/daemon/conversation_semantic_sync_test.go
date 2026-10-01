package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"

	"goodkind.io/clyde/internal/transcript"
)

type fakeConversationSemanticIndex struct {
	records      []conversation.StampedRecord
	messagesByID map[string][]transcript.Message
	loadOptions  []conversation.LoadOptions
	// tally is added into opts.HarnessTally on each load, standing in for what
	// a real parser would count as removed or withheld.
	tally transcript.HarnessStrips
}

func (idx *fakeConversationSemanticIndex) ListWithStamps(_ context.Context) ([]conversation.StampedRecord, error) {
	return append([]conversation.StampedRecord(nil), idx.records...), nil
}

func (idx *fakeConversationSemanticIndex) LoadMessagesWithOptions(record conversation.Record, opts conversation.LoadOptions) ([]transcript.Message, error) {
	idx.loadOptions = append(idx.loadOptions, opts)
	if opts.HarnessTally != nil {
		opts.HarnessTally.Injected += idx.tally.Injected
		opts.HarnessTally.System += idx.tally.System
	}
	messages, ok := idx.messagesByID[record.ID]
	if !ok {
		return nil, fmt.Errorf("messages for %s not found", record.ID)
	}
	return append([]transcript.Message(nil), messages...), nil
}

func semanticTestRecord(conversationID string) conversation.Record {
	return conversation.Record{
		ID:            conversationID,
		Provider:      conversation.ProviderCodex,
		NativeID:      conversationID,
		Lineage:       nil,
		Title:         "semantic test",
		WorkspaceRoot: "",
		ArtifactPath:  "/tmp/clyde-semantic-test.jsonl",
		ArtifactKind:  "jsonl",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      false,
	}
}

func semanticTestStamp(size int64, unixSeconds int64) conversation.FileStamp {
	return conversation.FileStamp{
		Size:  size,
		Mtime: time.Unix(unixSeconds, 0),
	}
}

func semanticTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// semanticTestContentKinds is the shipped default content selection, resolved
// the same way the daemon resolves it, so the sync tests project documents
// exactly as production does rather than under a selection invented for tests.
func semanticTestContentKinds() conversation.ContentKindSet {
	kinds, err := SemanticContentKinds(config.NewConfigWithDefaults().Conversation.Semantic)
	if err != nil {
		panic(err)
	}
	return kinds
}

func TestConversationSemanticFreshnessEmbeddedIsCumulativeCoverage(t *testing.T) {
	t.Parallel()
	freshness := newConversationSemanticFreshness()

	freshness.publish(conversationSemanticSyncStats{
		manifest:          1539,
		needed:            66,
		sentConversations: 10,
		documents:         3200,
		deferred:          0,
		failed:            0,
	})

	got := freshness.snapshot()
	if got.Embedded != 1539-66 {
		t.Fatalf("embedded = %d, want %d (manifest - needed), not the per-pass document count", got.Embedded, 1539-66)
	}
	if got.Embedded+got.Needed != got.Manifest {
		t.Fatalf("embedded(%d) + needed(%d) = %d, want manifest %d", got.Embedded, got.Needed, got.Embedded+got.Needed, got.Manifest)
	}
}
