package vectorsearch

import (
	"context"
	"errors"
	"slices"
	"testing"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/embedding"
)

type batchGateTestGate struct {
	entered  chan struct{}
	admit    chan struct{}
	released chan struct{}
}

type batchGateTestBackend struct {
	backend
}

func (*batchGateTestBackend) collectionPresent(context.Context, string) (bool, error) {
	return false, nil
}

func TestCanceledIdleWaitPreservesPendingAndCompletedFingerprints(t *testing.T) {
	gate := &batchGateTestGate{
		entered:  make(chan struct{}),
		admit:    make(chan struct{}),
		released: make(chan struct{}),
	}
	client := &Client{
		store:      &batchGateTestBackend{},
		checkpoint: newCheckpointStore(""),
		ingestGate: gate,
		embedder:   batchGateTestEmbedder{batched: make(chan struct{})},
	}
	const collectionID = "quiet"
	const complete = "codex:complete"
	const pending = "codex:pending"
	manifest := []semsearch.Fingerprint{
		{ConversationID: complete, Value: "complete-fingerprint"},
		{ConversationID: pending, Value: "pending-fingerprint"},
	}
	client.checkpoint.record(CollectionName(collectionID), complete, manifest[0].Value)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		_, err := client.UpsertConversationDocuments(
			ctx,
			collectionID,
			[]semsearch.SemDoc{
				{ConversationID: pending, Role: "user", Text: "pending conversation document"},
			},
			manifest,
		)
		errs <- err
	}()
	select {
	case <-gate.entered:
	case err := <-errs:
		t.Fatalf("upsert ended before admission: %v", err)
	}

	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("upsert error = %v, want canceled", err)
	}

	needed, err := client.SyncConversationManifest(t.Context(), collectionID, manifest)
	if err != nil || !slices.Equal(needed, []string{pending}) {
		t.Fatalf("needed = %v, error = %v, want only %s", needed, err, pending)
	}
}

func (gate *batchGateTestGate) Acquire(ctx context.Context) (func(), error) {
	close(gate.entered)
	select {
	case <-gate.admit:
		return func() { close(gate.released) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*batchGateTestGate) WaitingReason() string { return "user_active" }
func (*batchGateTestGate) Close()                {}

type batchGateTestEmbedder struct {
	batched chan struct{}
}

func (batchGateTestEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1}, nil
}

func (embedder batchGateTestEmbedder) EmbedBatch(
	context.Context,
	[]string,
) (embedding.BatchResult, error) {
	close(embedder.batched)

	return embedding.BatchResult{Vectors: [][]float32{{1}}}, nil
}

func TestMilvusIngestionWaitsWithoutBlockingQueryEmbedding(t *testing.T) {
	for _, cancelWhileWaiting := range []bool{false, true} {
		name := "admit"
		if cancelWhileWaiting {
			name = "cancel"
		}

		t.Run(
			name,
			func(t *testing.T) {
				gate := &batchGateTestGate{
					entered: make(
						chan struct{},
					),
					admit:    make(chan struct{}),
					released: make(chan struct{}),
				}
				embedder := batchGateTestEmbedder{batched: make(chan struct{})}
				client := &Client{embedder: embedder, ingestGate: gate, dimension: 1}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				errs := make(chan error, 1)
				go func() {
					_, _, err := client.vectorsFor(
						ctx,
						[]storedChunk{{Content: "test"}},
						nil,
						&ingestStats{},
					)
					errs <- err
				}()

				<-gate.entered
				select {
				case <-embedder.batched:
					t.Fatal("ingestion embedded before admission")
				default:
				}

				if _, err := client.embedQuery(ctx, "query"); err != nil {
					t.Fatalf("embed query while ingestion waits: %v", err)
				}

				if cancelWhileWaiting {
					cancel()
					if err := <-errs; !errors.Is(err, context.Canceled) {
						t.Fatalf("ingestion error = %v, want canceled", err)
					}

					select {
					case <-embedder.batched:
						t.Fatal("canceled work called the embedder")
					default:
					}

					return
				}

				close(gate.admit)
				if err := <-errs; err != nil {
					t.Fatal(err)
				}

				select {
				case <-gate.released:
				default:
					t.Fatal("ingestion did not release admission")
				}
			},
		)
	}
}
