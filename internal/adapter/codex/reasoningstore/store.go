package reasoningstore

import (
	"bufio"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/slogger"
)

// component is the slog component label emitted by every store event so
// log queries can pin findings to this package.
const component = "adapter.codex.reasoningstore"

// EncryptedBlob is one captured Codex reasoning item. Encrypted is
// server-signed opaque bytes preserved verbatim; the store does not
// decode or transform it. CreatedAt is UTC unix nanoseconds so cleanup
// loops can sort by age cheaply.
type EncryptedBlob struct {
	ChatKey   string `json:"chat_key"`
	ItemID    string `json:"item_id"`
	Encrypted string `json:"encrypted"`
	CreatedAt int64  `json:"created_at"`
}

// chatHandle owns the open append-mode file for a single chat key plus
// the per-chat mutex that serializes Puts on that chat. The map element
// holds the LRU list element so the LRU can be reordered cheaply on
// access.
type chatHandle struct {
	mu      sync.Mutex
	chatKey string
	path    string
	file    *os.File
	elem    *list.Element
}

// Store is the file-backed reasoning blob store. It maintains a bounded
// LRU of open append handles keyed by chat key, with a per-chat mutex
// for write serialization. The map mutex and per-chat mutex are never
// held at the same time.
type Store struct {
	rootDir string
	cap     int

	mapMu   sync.Mutex
	handles map[string]*chatHandle
	lru     *list.List
	closed  bool
}

// NewStore constructs a Store rooted at rootDir. The directory is
// created on demand with mode 0o700. An empty rootDir or a non-positive
// capacity is rejected.
func NewStore(rootDir string, capacity int) (*Store, error) {
	if strings.TrimSpace(rootDir) == "" {
		return nil, errors.New("reasoningstore: rootDir is required")
	}
	if capacity <= 0 {
		return nil, fmt.Errorf("reasoningstore: cap must be positive, got %d", capacity)
	}
	err := os.MkdirAll(rootDir, 0o700)
	if err != nil {
		slog.Warn("reasoningstore.new_store_failed",
			"component", component, "op", "new_store",
			"root_dir", rootDir, "err", err.Error())
		return nil, fmt.Errorf("reasoningstore: mkdir %q: %w", rootDir, err)
	}
	return &Store{
		rootDir: rootDir,
		cap:     capacity,
		mapMu:   sync.Mutex{},
		handles: make(map[string]*chatHandle, capacity),
		lru:     list.New(),
		closed:  false,
	}, nil
}

// pathFor returns the absolute on-disk path for the chat file. The
// chat key is sanitized through the canonical [slogger.SanitizeChatKey]
// rule so callers cannot escape rootDir.
func (s *Store) pathFor(chatKey string) (string, error) {
	sanitized := slogger.SanitizeChatKey(chatKey)
	if sanitized == "" {
		return "", fmt.Errorf("reasoningstore: chat key %q sanitizes to empty", chatKey)
	}
	return filepath.Join(s.rootDir, sanitized+".jsonl"), nil
}

// acquire returns a chatHandle for chatKey, opening the file in append
// mode if needed and inserting into the LRU. The caller must NOT hold
// any mutex when calling this; on return the per-chat mutex is held by
// the caller. release releases the per-chat mutex.
func (s *Store) acquire(ctx context.Context, chatKey string) (*chatHandle, error) {
	path, err := s.pathFor(chatKey)
	if err != nil {
		return nil, err
	}

	s.mapMu.Lock()
	if s.closed {
		s.mapMu.Unlock()
		return nil, errors.New("reasoningstore: store is closed")
	}
	existing, ok := s.handles[chatKey]
	if ok {
		s.lru.MoveToFront(existing.elem)
		s.mapMu.Unlock()
		existing.mu.Lock()
		return existing, nil
	}

	// Evict before insertion to keep the map at or below cap. Evicted
	// handles are closed without holding the map mutex below.
	var toClose []*chatHandle
	for s.lru.Len() >= s.cap {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		victim, vok := oldest.Value.(*chatHandle)
		if !vok {
			s.lru.Remove(oldest)
			continue
		}
		s.lru.Remove(oldest)
		delete(s.handles, victim.chatKey)
		toClose = append(toClose, victim)
	}
	s.mapMu.Unlock()

	for _, victim := range toClose {
		closeHandle(ctx, victim)
	}

	// Open the file outside the map lock so slow filesystems do not
	// block other chats. Another goroutine may have raced to insert the
	// same chat key in the meantime; if so, close ours and use theirs.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "reasoningstore.acquire_open_failed",
			"component", component, "op", "acquire",
			"path", path, "err", err.Error())
		return nil, fmt.Errorf("reasoningstore: open %q: %w", path, err)
	}

	handle := &chatHandle{
		mu:      sync.Mutex{},
		chatKey: chatKey,
		path:    path,
		file:    file,
		elem:    nil,
	}

	s.mapMu.Lock()
	if s.closed {
		s.mapMu.Unlock()
		_ = file.Close()
		return nil, errors.New("reasoningstore: store is closed")
	}
	winner, ok := s.handles[chatKey]
	if ok {
		s.lru.MoveToFront(winner.elem)
		s.mapMu.Unlock()
		_ = file.Close()
		winner.mu.Lock()
		return winner, nil
	}
	handle.elem = s.lru.PushFront(handle)
	s.handles[chatKey] = handle
	s.mapMu.Unlock()

	handle.mu.Lock()
	return handle, nil
}

// release releases the per-chat mutex.
func (s *Store) release(h *chatHandle) {
	h.mu.Unlock()
}

// closeHandle closes the file for an evicted handle. The per-chat mutex
// is taken to avoid closing under an active writer; this can never
// deadlock with the map mutex because callers do not hold the map
// mutex when invoking this helper.
func closeHandle(ctx context.Context, h *chatHandle) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file != nil {
		err := h.file.Close()
		if err != nil {
			slog.WarnContext(ctx, "reasoningstore.handle_close_failed",
				"component", component, "op", "close_handle",
				"path", h.path, "err", err.Error())
		}
		h.file = nil
	}
}

// Put appends a blob to the chat file as a single JSON-encoded line.
// The caller must populate ChatKey and ItemID; an empty value is
// rejected with a wrapped error.
func (s *Store) Put(ctx context.Context, b EncryptedBlob) error {
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "reasoningstore.put_canceled",
			"component", component, "op", "put",
			"chat_key", b.ChatKey, "err", err.Error())
		return fmt.Errorf("reasoningstore: put: %w", err)
	}
	if strings.TrimSpace(b.ChatKey) == "" {
		return errors.New("reasoningstore: put: chat key is required")
	}
	if strings.TrimSpace(b.ItemID) == "" {
		return errors.New("reasoningstore: put: item id is required")
	}
	encoded, err := json.Marshal(b)
	if err != nil {
		slog.WarnContext(ctx, "reasoningstore.put_marshal_failed",
			"component", component, "op", "put",
			"chat_key", b.ChatKey, "item_id", b.ItemID, "err", err.Error())
		return fmt.Errorf("reasoningstore: marshal blob: %w", err)
	}
	encoded = append(encoded, '\n')

	handle, err := s.acquire(ctx, b.ChatKey)
	if err != nil {
		return err
	}
	defer s.release(handle)

	if handle.file == nil {
		// Defensive: file was closed under us by an evictor that lost
		// the LRU race. Reopen in place.
		file, openErr := os.OpenFile(handle.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr != nil {
			slog.WarnContext(ctx, "reasoningstore.put_reopen_failed",
				"component", component, "op", "put",
				"path", handle.path, "err", openErr.Error())
			return fmt.Errorf("reasoningstore: reopen %q: %w", handle.path, openErr)
		}
		handle.file = file
	}

	_, err = handle.file.Write(encoded)
	if err != nil {
		slog.WarnContext(ctx, "reasoningstore.put_append_failed",
			"component", component, "op", "put",
			"path", handle.path, "chat_key", b.ChatKey,
			"item_id", b.ItemID, "err", err.Error())
		return fmt.Errorf("reasoningstore: append %q: %w", handle.path, err)
	}
	return nil
}

// Get returns the most recent blob for (chatKey, itemID), or nil with
// no error when the chat file or the item id is absent.
func (s *Store) Get(ctx context.Context, chatKey, itemID string) (*EncryptedBlob, error) {
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "reasoningstore.get_canceled",
			"component", component, "op", "get",
			"chat_key", chatKey, "err", err.Error())
		return nil, fmt.Errorf("reasoningstore: get: %w", err)
	}
	if strings.TrimSpace(chatKey) == "" {
		return nil, errors.New("reasoningstore: get: chat key is required")
	}
	if strings.TrimSpace(itemID) == "" {
		return nil, errors.New("reasoningstore: get: item id is required")
	}
	path, err := s.pathFor(chatKey)
	if err != nil {
		return nil, err
	}

	// Serialize Get against Put for the same chat by acquiring the
	// per-chat handle. This keeps Put's append visible to Get on
	// platforms with weak append-vs-read ordering and avoids reading a
	// torn line.
	handle, err := s.acquire(ctx, chatKey)
	if err != nil {
		return nil, err
	}
	defer s.release(handle)

	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		slog.WarnContext(ctx, "reasoningstore.get_open_failed",
			"component", component, "op", "get",
			"path", path, "err", err.Error())
		return nil, fmt.Errorf("reasoningstore: open %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	var latest *EncryptedBlob
	scanner := bufio.NewScanner(file)
	// Reasoning blobs can exceed the default 64 KiB scanner buffer.
	// Allow up to 8 MiB per line.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var blob EncryptedBlob
		decodeErr := json.Unmarshal(line, &blob)
		if decodeErr != nil {
			slog.WarnContext(ctx, "reasoningstore.get_decode_failed",
				"component", component, "op", "get",
				"path", path, "err", decodeErr.Error())
			return nil, fmt.Errorf("reasoningstore: decode line in %q: %w", path, decodeErr)
		}
		if blob.ItemID == itemID {
			copied := blob
			latest = &copied
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		slog.WarnContext(ctx, "reasoningstore.get_scan_failed",
			"component", component, "op", "get",
			"path", path, "err", scanErr.Error())
		return nil, fmt.Errorf("reasoningstore: scan %q: %w", path, scanErr)
	}
	return latest, nil
}

// ListChats returns the chat keys with at least one stored blob. The
// returned keys are the sanitized on-disk filenames (without the
// .jsonl suffix), which is the same form callers should pass back to
// EvictChat.
func (s *Store) ListChats(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "reasoningstore.list_canceled",
			"component", component, "op", "list", "err", err.Error())
		return nil, fmt.Errorf("reasoningstore: list: %w", err)
	}
	entries, err := os.ReadDir(s.rootDir)
	if err != nil {
		slog.WarnContext(ctx, "reasoningstore.list_read_dir_failed",
			"component", component, "op", "list",
			"root_dir", s.rootDir, "err", err.Error())
		return nil, fmt.Errorf("reasoningstore: read dir %q: %w", s.rootDir, err)
	}
	chats := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		chats = append(chats, strings.TrimSuffix(name, ".jsonl"))
	}
	return chats, nil
}

// EvictChat removes a chat's file entirely. The handle, if any, is
// removed from the LRU and closed first. EvictChat is the only path
// that deletes on-disk data; LRU eviction never deletes files.
func (s *Store) EvictChat(ctx context.Context, chatKey string) error {
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "reasoningstore.evict_canceled",
			"component", component, "op", "evict",
			"chat_key", chatKey, "err", err.Error())
		return fmt.Errorf("reasoningstore: evict: %w", err)
	}
	if strings.TrimSpace(chatKey) == "" {
		return errors.New("reasoningstore: evict: chat key is required")
	}
	path, err := s.pathFor(chatKey)
	if err != nil {
		return err
	}

	s.mapMu.Lock()
	handle, ok := s.handles[chatKey]
	if ok {
		s.lru.Remove(handle.elem)
		delete(s.handles, chatKey)
	}
	s.mapMu.Unlock()

	if ok {
		closeHandle(ctx, handle)
	}

	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		slog.WarnContext(ctx, "reasoningstore.evict_remove_failed",
			"component", component, "op", "evict",
			"path", path, "chat_key", chatKey, "err", removeErr.Error())
		return fmt.Errorf("reasoningstore: remove %q: %w", path, removeErr)
	}
	slog.InfoContext(ctx, "reasoningstore.chat_evicted",
		"component", component, "op", "evict",
		"path", path, "chat_key", chatKey, "had_handle", ok)
	return nil
}

// Close flushes and releases all open file handles. After Close the
// store rejects further operations.
func (s *Store) Close() error {
	s.mapMu.Lock()
	if s.closed {
		s.mapMu.Unlock()
		return nil
	}
	s.closed = true
	handles := make([]*chatHandle, 0, len(s.handles))
	for _, h := range s.handles {
		handles = append(handles, h)
	}
	s.handles = make(map[string]*chatHandle)
	s.lru.Init()
	s.mapMu.Unlock()

	var errs []error
	for _, h := range handles {
		h.mu.Lock()
		if h.file != nil {
			err := h.file.Close()
			if err != nil {
				errs = append(errs, fmt.Errorf("close %q: %w", h.path, err))
			}
			h.file = nil
		}
		h.mu.Unlock()
	}
	if len(errs) > 0 {
		joined := errors.Join(errs...)
		slog.Warn("reasoningstore.close_errors",
			"component", component, "op", "close",
			"count", len(errs), "err", joined.Error())
		return fmt.Errorf("reasoningstore: close: %w", joined)
	}
	return nil
}

// compile-time guard so [io.Closer] is satisfied by *Store.
var _ io.Closer = (*Store)(nil)
