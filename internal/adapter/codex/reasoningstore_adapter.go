package codex

import (
	"context"
	"fmt"

	"goodkind.io/clyde/internal/adapter/codex/reasoningstore"
)

// reasoningStoreAdapter wraps a *reasoningstore.Store so it satisfies the
// codex package's [ReasoningBlobStore] interface. The adapter keeps the codex
// package's runtime types (ReasoningBlob) decoupled from the on-disk storage
// types (reasoningstore.EncryptedBlob); they have identical shapes today but
// the boundary lets either side evolve without churning the other.
type reasoningStoreAdapter struct {
	inner *reasoningstore.Store
}

// NewReasoningStoreAdapter constructs the adapter. nil store yields nil so
// the dispatcher can pass it straight through to ProviderOptions without a
// guard at the call site.
func NewReasoningStoreAdapter(store *reasoningstore.Store) ReasoningBlobStore {
	if store == nil {
		return nil
	}
	return &reasoningStoreAdapter{inner: store}
}

// Put forwards to the underlying store, mapping the codex package's
// ReasoningBlob into reasoningstore.EncryptedBlob.
func (a *reasoningStoreAdapter) Put(ctx context.Context, blob ReasoningBlob) error {
	if a == nil || a.inner == nil {
		return nil
	}
	if err := a.inner.Put(ctx, reasoningstore.EncryptedBlob{
		ChatKey:   blob.ChatKey,
		ItemID:    blob.ItemID,
		Encrypted: blob.Encrypted,
		CreatedAt: blob.CreatedAt,
	}); err != nil {
		return fmt.Errorf("reasoningstore put: %w", err)
	}
	return nil
}
