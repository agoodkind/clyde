package codex

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/correlation"
)

// fakeReasoningStore captures Put calls for assertion. Matches the codex
// package's [ReasoningBlobStore] interface so DirectConfig can hold it.
type fakeReasoningStore struct {
	mu    sync.Mutex
	puts  []ReasoningBlob
	fail  bool
	calls int
}

func (f *fakeReasoningStore) Put(_ context.Context, blob ReasoningBlob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return errors.New("fake store: induced failure")
	}
	f.puts = append(f.puts, blob)
	return nil
}

func (f *fakeReasoningStore) snapshot() []ReasoningBlob {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ReasoningBlob, len(f.puts))
	copy(out, f.puts)
	return out
}

func TestCaptureReasoningBlobsPersistsReasoningItems(t *testing.T) {
	t.Parallel()
	store := &fakeReasoningStore{mu: sync.Mutex{}, puts: nil, fail: false, calls: 0}
	cfg := DirectConfig{}
	cfg.Correlation = correlation.Context{ChatKey: "chat-abc"}
	cfg.ReasoningStore = store
	cfg.RoundTripEncrypted = RoundTripEncryptedRoundTrip
	cfg.Log = slog.Default()
	result := RunResult{}
	result.OutputItems = []map[string]any{
		{"type": "reasoning", "id": "rs_one", "encrypted_content": "blob-one"},
		{"type": "message", "id": "msg_skip"},
		{"type": "reasoning", "id": "rs_two", "encrypted_content": "blob-two"},
	}
	captureReasoningBlobs(context.Background(), cfg, result)
	got := store.snapshot()
	if len(got) != 2 {
		t.Fatalf("want 2 puts, got %d: %#v", len(got), got)
	}
	if got[0].ItemID != "rs_one" || got[0].Encrypted != "blob-one" || got[0].ChatKey != "chat-abc" {
		t.Fatalf("first put mismatch: %#v", got[0])
	}
	if got[1].ItemID != "rs_two" || got[1].Encrypted != "blob-two" {
		t.Fatalf("second put mismatch: %#v", got[1])
	}
	if got[0].CreatedAt == 0 || got[1].CreatedAt == 0 {
		t.Fatalf("CreatedAt should be set: %#v %#v", got[0], got[1])
	}
}

func TestCaptureReasoningBlobsSkipsWhenStoreNil(t *testing.T) {
	t.Parallel()
	cfg := DirectConfig{}
	cfg.Correlation = correlation.Context{ChatKey: "chat-abc"}
	cfg.RoundTripEncrypted = RoundTripEncryptedRoundTrip
	cfg.Log = slog.Default()
	result := RunResult{}
	result.OutputItems = []map[string]any{
		{"type": "reasoning", "id": "rs_one", "encrypted_content": "blob-one"},
	}
	captureReasoningBlobs(context.Background(), cfg, result)
}

func TestCaptureReasoningBlobsSkipsWhenChatKeyEmpty(t *testing.T) {
	t.Parallel()
	store := &fakeReasoningStore{mu: sync.Mutex{}, puts: nil, fail: false, calls: 0}
	cfg := DirectConfig{}
	cfg.Correlation = correlation.Context{ChatKey: ""}
	cfg.ReasoningStore = store
	cfg.RoundTripEncrypted = RoundTripEncryptedRoundTrip
	cfg.Log = slog.Default()
	result := RunResult{}
	result.OutputItems = []map[string]any{
		{"type": "reasoning", "id": "rs_one", "encrypted_content": "blob-one"},
	}
	captureReasoningBlobs(context.Background(), cfg, result)
	if got := store.snapshot(); len(got) != 0 {
		t.Fatalf("want 0 puts when chat_key empty, got %d", len(got))
	}
}

func TestCaptureReasoningBlobsSkipsWhenRoundTripDropped(t *testing.T) {
	t.Parallel()
	store := &fakeReasoningStore{mu: sync.Mutex{}, puts: nil, fail: false, calls: 0}
	cfg := DirectConfig{}
	cfg.Correlation = correlation.Context{ChatKey: "chat-abc"}
	cfg.ReasoningStore = store
	cfg.RoundTripEncrypted = RoundTripEncryptedDrop
	cfg.Log = slog.Default()
	result := RunResult{}
	result.OutputItems = []map[string]any{
		{"type": "reasoning", "id": "rs_one", "encrypted_content": "blob-one"},
	}
	captureReasoningBlobs(context.Background(), cfg, result)
	if got := store.snapshot(); len(got) != 0 {
		t.Fatalf("want 0 puts when round_trip_encrypted=drop, got %d", len(got))
	}
}

func TestCaptureReasoningBlobsSkipsItemsWithoutEncryptedContent(t *testing.T) {
	t.Parallel()
	store := &fakeReasoningStore{mu: sync.Mutex{}, puts: nil, fail: false, calls: 0}
	cfg := DirectConfig{}
	cfg.Correlation = correlation.Context{ChatKey: "chat-abc"}
	cfg.ReasoningStore = store
	cfg.RoundTripEncrypted = RoundTripEncryptedRoundTrip
	cfg.Log = slog.Default()
	result := RunResult{}
	result.OutputItems = []map[string]any{
		{"type": "reasoning", "id": "rs_no_blob"},
		{"type": "reasoning", "id": "rs_empty_blob", "encrypted_content": ""},
		{"type": "reasoning", "id": "rs_blank_blob", "encrypted_content": "   "},
	}
	captureReasoningBlobs(context.Background(), cfg, result)
	if got := store.snapshot(); len(got) != 0 {
		t.Fatalf("reasoning items without encrypted_content should be skipped, got %d", len(got))
	}
}

func TestCaptureReasoningBlobsSwallowsPutErrors(t *testing.T) {
	t.Parallel()
	store := &fakeReasoningStore{mu: sync.Mutex{}, puts: nil, fail: true, calls: 0}
	cfg := DirectConfig{}
	cfg.Correlation = correlation.Context{ChatKey: "chat-abc"}
	cfg.ReasoningStore = store
	cfg.RoundTripEncrypted = RoundTripEncryptedRoundTrip
	cfg.Log = slog.Default()
	result := RunResult{}
	result.OutputItems = []map[string]any{
		{"type": "reasoning", "id": "rs_one", "encrypted_content": "blob-one"},
	}
	// Best-effort: errors are logged at WARN; the function must not panic
	// or propagate. Verifying the call was attempted is enough.
	captureReasoningBlobs(context.Background(), cfg, result)
	if store.calls != 1 {
		t.Fatalf("want 1 attempted put, got %d", store.calls)
	}
}

func TestCaptureReasoningBlobsTreatsEmptyRoundTripAsRoundTripDefault(t *testing.T) {
	t.Parallel()
	store := &fakeReasoningStore{mu: sync.Mutex{}, puts: nil, fail: false, calls: 0}
	cfg := DirectConfig{}
	cfg.Correlation = correlation.Context{ChatKey: "chat-abc"}
	cfg.ReasoningStore = store
	cfg.RoundTripEncrypted = ""
	cfg.Log = slog.Default()
	result := RunResult{}
	result.OutputItems = []map[string]any{
		{"type": "reasoning", "id": "rs_one", "encrypted_content": "blob-one"},
	}
	captureReasoningBlobs(context.Background(), cfg, result)
	if got := store.snapshot(); len(got) != 1 {
		t.Fatalf("empty mode must default to round_trip and persist; got %d puts", len(got))
	}
}
