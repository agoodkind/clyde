package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

// reconcileAfterCloseTimeout bounds a reopen attempt against the closed local
// Milvus port.
const reconcileAfterCloseTimeout = 5 * time.Second

// TestEmbeddedReconcileAfterCloseOpensNothing attaches an embedded worker
// state with an open store to the reconcile gate and closes the store while
// the gate still returns the worker. This is the order in which a reconcile
// request reads the worker before the worker detaches and closes its store.
// The reconcile request must fail with errEmbeddedReconcileUnavailable and
// must leave the worker without a store. The Milvus address is a closed local
// port.
func TestEmbeddedReconcileAfterCloseOpensNothing(t *testing.T) {
	store, _, semantic := openUnmanagedBlockedTestStore(t)
	semantic.MilvusAddress = "localhost:1"
	semantic.MilvusDatabase = "clyde_reconcile_after_close"
	semantic.MilvusCollection = "vectors"
	index := conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{})
	worker := newEmbeddedConversationSync(semantic, conversationSemanticOutboxPath(semantic.PoolID), newEmbeddedSemanticStatus(), index)
	worker.store = store
	gate := newEmbeddedReconcileGate(nil)
	gate.attach(worker)

	worker.closeStore(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), reconcileAfterCloseTimeout)
	defer cancel()
	_, err := gate.reconcile(ctx, "codex:any-conversation")
	if !errors.Is(err, errEmbeddedReconcileUnavailable) {
		t.Fatalf("reconcile after close error = %v, want errEmbeddedReconcileUnavailable", err)
	}
	if worker.store != nil {
		t.Fatal("reconcile after close opened a store")
	}
}
