package reasoningstore

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStore(t *testing.T, cap int) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir, cap)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func TestPutGetSingle(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	want := EncryptedBlob{
		ChatKey:   "chat-1",
		ItemID:    "item-a",
		Encrypted: "opaque-blob-bytes",
		CreatedAt: time.Now().UTC().UnixNano(),
	}
	err := store.Put(ctx, want)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(ctx, "chat-1", "item-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if *got != want {
		t.Fatalf("Get mismatch: got %+v want %+v", *got, want)
	}
}

func TestGetReturnsLatestForItemID(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	first := EncryptedBlob{
		ChatKey:   "chat-2",
		ItemID:    "rs_1",
		Encrypted: "first",
		CreatedAt: 1,
	}
	second := EncryptedBlob{
		ChatKey:   "chat-2",
		ItemID:    "rs_1",
		Encrypted: "second",
		CreatedAt: 2,
	}
	if err := store.Put(ctx, first); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if err := store.Put(ctx, second); err != nil {
		t.Fatalf("Put second: %v", err)
	}
	got, err := store.Get(ctx, "chat-2", "rs_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Encrypted != "second" || got.CreatedAt != 2 {
		t.Fatalf("Get latest mismatch: got %+v", got)
	}
}

func TestGetMissingChatReturnsNilNoError(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	got, err := store.Get(ctx, "no-such-chat", "rs_x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestGetMissingItemIDReturnsNilNoError(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	err := store.Put(ctx, EncryptedBlob{
		ChatKey:   "chat-3",
		ItemID:    "rs_present",
		Encrypted: "x",
		CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, "chat-3", "rs_absent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestSanitizesChatKeyForFilesystem(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	// '/' and '.' are not in [A-Za-z0-9_-] and become '_'.
	chatKey := "team/proj.beta"
	wantFile := "team_proj_beta.jsonl"

	err := store.Put(ctx, EncryptedBlob{
		ChatKey:   chatKey,
		ItemID:    "rs_1",
		Encrypted: "x",
		CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	path := filepath.Join(store.rootDir, wantFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected sanitized file %q, stat err: %v", path, err)
	}
}

func TestPathTraversalEscape(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	chatKey := "../../etc/passwd"
	wantFile := "______etc_passwd.jsonl"

	err := store.Put(ctx, EncryptedBlob{
		ChatKey:   chatKey,
		ItemID:    "rs_1",
		Encrypted: "x",
		CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	path := filepath.Join(store.rootDir, wantFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected sanitized file %q, stat err: %v", path, err)
	}
	if info.IsDir() {
		t.Fatalf("expected file at %q, got directory", path)
	}

	// Confirm nothing landed outside rootDir under the escape path.
	parent := filepath.Dir(store.rootDir)
	if _, err := os.Stat(filepath.Join(parent, "etc")); !os.IsNotExist(err) {
		t.Fatalf("unexpected escape: %v", err)
	}
}

func TestConcurrentPutsAreLineValid(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	const goroutines = 16
	const perGoroutine = 25
	const total = goroutines * perGoroutine
	const chatKey = "chat-concurrent"

	start := time.Now()

	var wg sync.WaitGroup
	var seq atomic.Int64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				err := store.Put(ctx, EncryptedBlob{
					ChatKey:   chatKey,
					ItemID:    formatID(g, i),
					Encrypted: "blob",
					CreatedAt: seq.Add(1),
				})
				if err != nil {
					t.Errorf("Put g=%d i=%d: %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	elapsed := time.Since(start)
	t.Logf("16x25 concurrent puts in %s", elapsed)

	path := filepath.Join(store.rootDir, "chat-concurrent.jsonl")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	count := 0
	seenIDs := make(map[string]bool, total)
	seenCreatedAt := make(map[int64]bool, total)
	// perGoroutineOrder tracks the last index seen for each goroutine.
	// Within a goroutine, Puts are serial so the on-disk order must be
	// monotonic for that goroutine's slice of lines. Across goroutines
	// the per-chat mutex orders writes by lock-acquire time, which is
	// independent of the seq counter assigned before locking.
	perGoroutineLastIndex := make(map[int]int)
	for scanner.Scan() {
		var blob EncryptedBlob
		err := json.Unmarshal(scanner.Bytes(), &blob)
		if err != nil {
			t.Fatalf("invalid JSON line %d: %v: %q", count, err, scanner.Bytes())
		}
		if seenIDs[blob.ItemID] {
			t.Fatalf("duplicate ItemID %q at line %d", blob.ItemID, count)
		}
		seenIDs[blob.ItemID] = true
		if seenCreatedAt[blob.CreatedAt] {
			t.Fatalf("duplicate CreatedAt %d at line %d", blob.CreatedAt, count)
		}
		seenCreatedAt[blob.CreatedAt] = true
		g, i := parseGoroutineIndex(t, blob.ItemID)
		last, ok := perGoroutineLastIndex[g]
		if ok && i <= last {
			t.Fatalf("non-monotonic per-goroutine order: g=%d last=%d cur=%d", g, last, i)
		}
		perGoroutineLastIndex[g] = i
		count++
	}
	if scanErr := scanner.Err(); scanErr != nil {
		t.Fatalf("scan: %v", scanErr)
	}
	if count != total {
		t.Fatalf("expected %d lines, got %d", total, count)
	}
}

func parseGoroutineIndex(t *testing.T, id string) (int, int) {
	t.Helper()
	// id format: "rs_g<G>_i<I>"
	rest, ok := strings.CutPrefix(id, "rs_g")
	if !ok {
		t.Fatalf("unexpected id %q", id)
	}
	parts := strings.SplitN(rest, "_i", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected id %q", id)
	}
	return atoi(t, parts[0]), atoi(t, parts[1])
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("non-digit in %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func formatID(goroutine, index int) string {
	var b strings.Builder
	b.WriteString("rs_g")
	b.WriteString(itoa(goroutine))
	b.WriteString("_i")
	b.WriteString(itoa(index))
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

func TestLRUEvictsButDoesNotDeleteFile(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 2)
	ctx := context.Background()

	put := func(chat string) {
		t.Helper()
		err := store.Put(ctx, EncryptedBlob{
			ChatKey:   chat,
			ItemID:    "rs_only",
			Encrypted: "blob",
			CreatedAt: 1,
		})
		if err != nil {
			t.Fatalf("Put %s: %v", chat, err)
		}
	}

	put("chat-A")
	put("chat-B")
	put("chat-C") // evicts chat-A from LRU

	// chat-A's file must still exist on disk.
	pathA := filepath.Join(store.rootDir, "chat-A.jsonl")
	if _, err := os.Stat(pathA); err != nil {
		t.Fatalf("chat-A file missing after LRU eviction: %v", err)
	}

	// LRU map no longer holds chat-A.
	store.mapMu.Lock()
	_, present := store.handles["chat-A"]
	store.mapMu.Unlock()
	if present {
		t.Fatal("chat-A still in handles map after eviction")
	}

	// Subsequent Get must reopen and return the original blob.
	got, err := store.Get(ctx, "chat-A", "rs_only")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Encrypted != "blob" {
		t.Fatalf("Get after eviction mismatch: %+v", got)
	}
}

func TestEvictChatDeletesFile(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	err := store.Put(ctx, EncryptedBlob{
		ChatKey:   "chat-evict",
		ItemID:    "rs_1",
		Encrypted: "x",
		CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	path := filepath.Join(store.rootDir, "chat-evict.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-evict stat: %v", err)
	}

	if err := store.EvictChat(ctx, "chat-evict"); err != nil {
		t.Fatalf("EvictChat: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file removed, got err=%v", err)
	}

	store.mapMu.Lock()
	_, present := store.handles["chat-evict"]
	store.mapMu.Unlock()
	if present {
		t.Fatal("handle still present after EvictChat")
	}
}

func TestCloseFlushesAndReleases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir, 4)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()

	for _, chat := range []string{"c1", "c2", "c3"} {
		err := store.Put(ctx, EncryptedBlob{
			ChatKey:   chat,
			ItemID:    "rs_1",
			Encrypted: "x",
			CreatedAt: 1,
		})
		if err != nil {
			t.Fatalf("Put %s: %v", chat, err)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store.mapMu.Lock()
	openCount := 0
	for _, h := range store.handles {
		if h.file != nil {
			openCount++
		}
	}
	mapLen := len(store.handles)
	listLen := store.lru.Len()
	store.mapMu.Unlock()

	if openCount != 0 {
		t.Fatalf("expected zero open files after Close, got %d", openCount)
	}
	if mapLen != 0 {
		t.Fatalf("expected empty handles map after Close, got %d", mapLen)
	}
	if listLen != 0 {
		t.Fatalf("expected empty LRU list after Close, got %d", listLen)
	}

	// Second Close is a no-op.
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Subsequent Put fails cleanly.
	err = store.Put(ctx, EncryptedBlob{ChatKey: "c1", ItemID: "rs_after", Encrypted: "x", CreatedAt: 1})
	if err == nil {
		t.Fatal("expected error on Put after Close")
	}
}

func TestListChatsReturnsKnownKeys(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, 4)
	ctx := context.Background()

	for _, chat := range []string{"alpha", "beta"} {
		err := store.Put(ctx, EncryptedBlob{
			ChatKey:   chat,
			ItemID:    "rs_1",
			Encrypted: "x",
			CreatedAt: 1,
		})
		if err != nil {
			t.Fatalf("Put %s: %v", chat, err)
		}
	}
	chats, err := store.ListChats(ctx)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range chats {
		seen[c] = true
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Fatalf("missing chats in %v", chats)
	}
}

func TestNewStoreRejectsBadInput(t *testing.T) {
	t.Parallel()
	if _, err := NewStore("", 4); err == nil {
		t.Fatal("expected error for empty rootDir")
	}
	if _, err := NewStore(t.TempDir(), 0); err == nil {
		t.Fatal("expected error for zero cap")
	}
}
