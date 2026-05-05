package codex

import (
	"context"
	"fmt"
	"log/slog"

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

// NewReasoningStoreGetter returns the read-side adapter for store, or nil
// when store is nil. The returned value satisfies [ReasoningStoreGetter]
// and is safe to pass directly into [ProviderOptions.ReasoningStoreGet].
func NewReasoningStoreGetter(store *reasoningstore.Store) ReasoningStoreGetter {
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

// Get forwards to the underlying store, mapping
// reasoningstore.EncryptedBlob back into the codex package's ReasoningBlob.
// A nil blob with a nil error is the documented "miss" signal and is
// passed through unchanged.
func (a *reasoningStoreAdapter) Get(ctx context.Context, chatKey, itemID string) (*ReasoningBlob, error) {
	if a == nil || a.inner == nil {
		return nil, nil
	}
	got, err := a.inner.Get(ctx, chatKey, itemID)
	if err != nil {
		slog.WarnContext(ctx, "adapter.codex.reasoningstore.get_failed",
			"component", "adapter",
			"subcomponent", "codex",
			"chat_key", chatKey,
			"item_id", itemID,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("reasoningstore get: %w", err)
	}
	if got == nil {
		return nil, nil
	}
	return &ReasoningBlob{
		ChatKey:   got.ChatKey,
		ItemID:    got.ItemID,
		Encrypted: got.Encrypted,
		CreatedAt: got.CreatedAt,
	}, nil
}
