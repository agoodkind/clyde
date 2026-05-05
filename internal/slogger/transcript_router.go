package slogger

// File transcript_router contains the per-chat slog tee. Records that carry a
// chat_key attribute are appended to chats/<chat_key>.jsonl alongside their
// existing concern files. One file per chat; every turn of the chat appends
// to the same file. Only an explicit allowlist of msg names is teed.

import (
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
	"time"
)

// TranscriptMode controls body redaction for teed records.
type TranscriptMode string

const (
	// TranscriptModeSummary strips known body fields before write.
	TranscriptModeSummary TranscriptMode = "summary"
	// TranscriptModeRaw passes records through verbatim.
	TranscriptModeRaw TranscriptMode = "raw"
)

const (
	chatKeyAttr              = "chat_key"
	requestIDAttr            = "request_id"
	transcriptDefaultPoolCap = 64
	transcriptFilePerm       = 0o600
	transcriptDirPerm        = 0o755
)

// transcriptAllowlist is the set of slog message names that get teed into the
// per-chat file. Everything else stays in the per-concern file only.
var transcriptAllowlist = map[string]struct{}{
	"adapter.chat.raw":            {},
	"adapter.chat.ingress":        {},
	"adapter.chat.discovery":      {},
	"adapter.chat.received":       {},
	"anthropic.messages.request":  {},
	"codex.responses.request":     {},
	"adapter.chat.completed":      {},
	"adapter.event.delta_summary": {},
}

// transcriptStrippedBodyFields is the summary-mode redaction list. These keys
// are dropped from the cloned record before writing to the per-chat file.
var transcriptStrippedBodyFields = map[string]struct{}{
	"body":     {},
	"body_b64": {},
}

// TranscriptRouterConfig configures the per-chat file router.
type TranscriptRouterConfig struct {
	// Root is the directory under which <chat_key>.jsonl files are written.
	// Typically <state>/clyde/logs/chats.
	Root string
	// Mode is summary or raw.
	Mode TranscriptMode
	// PoolCap caps simultaneously open file handles. Zero falls back to
	// transcriptDefaultPoolCap.
	PoolCap int
	// Now is injectable for tests. Defaults to time.Now. Retained for
	// future per-record fields; the file path itself does not depend on a
	// timestamp.
	Now func() time.Time
}

// TranscriptRouter is an [slog.Handler] that tees allowlisted records into
// per-chat JSONL files. Records without a chat_key, with a sanitized chat_key
// that is empty, or with a msg outside the allowlist are dropped silently.
//
// State is held behind a pointer so WithAttrs / WithGroup clones share the
// LRU file-handle pool with the root router.
type TranscriptRouter struct {
	state *transcriptRouterState
	attrs []slog.Attr
	group string
}

type transcriptRouterState struct {
	cfg   TranscriptRouterConfig
	mu    sync.Mutex
	cache map[string]*transcriptHandle
	order []string // LRU; tail is most recent
}

type transcriptHandle struct {
	path string
	file *os.File
}

// NewTranscriptRouter constructs a router with the given config. The router
// implements [slog.Handler] and is safe for concurrent use.
func NewTranscriptRouter(cfg TranscriptRouterConfig) *TranscriptRouter {
	if cfg.PoolCap <= 0 {
		cfg.PoolCap = transcriptDefaultPoolCap
	}
	if cfg.Mode == "" {
		cfg.Mode = TranscriptModeSummary
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &TranscriptRouter{
		state: &transcriptRouterState{
			cfg:   cfg,
			cache: make(map[string]*transcriptHandle, cfg.PoolCap),
		},
	}
}

// Enabled implements [slog.Handler]. The router is unconditionally enabled at
// the level the caller produced; the allowlist filters by msg name later.
func (*TranscriptRouter) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

// Handle implements [slog.Handler]. Non-allowlisted msgs and records without a
// resolvable chat_key are dropped without error. Write failures are returned
// so the upstream tee handler can surface them.
//
// The per-chat file path is <Root>/<sanitized_chat_key>.jsonl. Every turn of
// the same chat appends to the same file; turn boundaries are visible in the
// records themselves via request_id and msg fields.
func (r *TranscriptRouter) Handle(ctx context.Context, record slog.Record) error {
	if _, ok := transcriptAllowlist[record.Message]; !ok {
		return nil
	}
	chatKey, _ := r.extractKeys(record)
	if chatKey == "" {
		return nil
	}
	sanitized := sanitizeChatKey(chatKey)
	if sanitized == "" {
		return nil
	}

	relPath := sanitized + ".jsonl"

	payload, err := r.encodeRecord(record)
	if err != nil {
		slog.WarnContext(ctx, "transcript.encode_failed",
			"component", "transcript-router",
			"err", err,
			"msg_name", record.Message,
		)
		return fmt.Errorf("transcript: encode record: %w", err)
	}

	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	handle, err := r.state.acquireLocked(relPath)
	if err != nil {
		return err
	}
	_, err = handle.file.Write(payload)
	if err != nil {
		slog.WarnContext(ctx, "transcript.write_failed",
			"component", "transcript-router",
			"err", err,
			"path", handle.path,
		)
		return fmt.Errorf("transcript: write %s: %w", handle.path, err)
	}
	return nil
}

// WithAttrs implements [slog.Handler]. The clone shares the underlying LRU pool
// state so all derived loggers route into the same per-chat files.
func (r *TranscriptRouter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TranscriptRouter{
		state: r.state,
		attrs: append(append([]slog.Attr(nil), r.attrs...), attrs...),
		group: r.group,
	}
}

// WithGroup implements [slog.Handler]. Groups are tracked but not used for
// routing; the router only cares about top-level chat_key/request_id attrs.
func (r *TranscriptRouter) WithGroup(name string) slog.Handler {
	return &TranscriptRouter{
		state: r.state,
		attrs: append([]slog.Attr(nil), r.attrs...),
		group: name,
	}
}

// Close flushes and closes every cached handle. Safe to call once on shutdown.
func (r *TranscriptRouter) Close() error {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	var errs []error
	for _, h := range r.state.cache {
		err := h.file.Close()
		if err != nil {
			slog.Warn("transcript.close_failed",
				"component", "transcript-router",
				"path", h.path,
				"err", err,
			)
			errs = append(errs, err)
		}
	}
	r.state.cache = map[string]*transcriptHandle{}
	r.state.order = nil
	return errors.Join(errs...)
}

func (r *TranscriptRouter) extractKeys(record slog.Record) (chatKey, requestID string) {
	for _, attr := range r.attrs {
		switch attr.Key {
		case chatKeyAttr:
			chatKey = attr.Value.String()
		case requestIDAttr:
			requestID = attr.Value.String()
		}
	}
	record.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case chatKeyAttr:
			chatKey = attr.Value.String()
		case requestIDAttr:
			requestID = attr.Value.String()
		}
		return true
	})
	return strings.TrimSpace(chatKey), strings.TrimSpace(requestID)
}

func (r *TranscriptRouter) encodeRecord(record slog.Record) ([]byte, error) {
	out := make(map[string]json.RawMessage, record.NumAttrs()+4)
	tsBytes, err := json.Marshal(record.Time.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("transcript: marshal time: %w", err)
	}
	out["time"] = tsBytes
	lvlBytes, err := json.Marshal(record.Level.String())
	if err != nil {
		return nil, fmt.Errorf("transcript: marshal level: %w", err)
	}
	out["level"] = lvlBytes
	msgBytes, err := json.Marshal(record.Message)
	if err != nil {
		return nil, fmt.Errorf("transcript: marshal msg: %w", err)
	}
	out["msg"] = msgBytes

	addAttr := func(attr slog.Attr) error {
		if r.state.cfg.Mode == TranscriptModeSummary {
			if _, drop := transcriptStrippedBodyFields[attr.Key]; drop {
				return nil
			}
		}
		raw, err := json.Marshal(attr.Value.Any())
		if err != nil {
			slog.Warn("transcript.marshal_attr_failed",
				"component", "transcript-router",
				"attr_key", attr.Key,
				"err", err,
			)
			return fmt.Errorf("transcript: marshal attr %q: %w", attr.Key, err)
		}
		out[attr.Key] = raw
		return nil
	}
	for _, attr := range r.attrs {
		if attr.Key == "" {
			continue
		}
		err = addAttr(attr)
		if err != nil {
			return nil, err
		}
	}
	var attrErr error
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "" {
			return true
		}
		err := addAttr(attr)
		if err != nil {
			attrErr = err
			return false
		}
		return true
	})
	if attrErr != nil {
		return nil, attrErr
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		slog.Warn("transcript.marshal_record_failed",
			"component", "transcript-router",
			"err", err,
		)
		return nil, fmt.Errorf("transcript: marshal record: %w", err)
	}
	return append(encoded, '\n'), nil
}

func (s *transcriptRouterState) acquireLocked(relPath string) (*transcriptHandle, error) {
	if h, ok := s.cache[relPath]; ok {
		s.touchLocked(relPath)
		return h, nil
	}
	if len(s.cache) >= s.cfg.PoolCap {
		err := s.evictOldestLocked()
		if err != nil {
			return nil, err
		}
	}
	full := filepath.Join(s.cfg.Root, relPath)
	err := os.MkdirAll(filepath.Dir(full), transcriptDirPerm)
	if err != nil {
		slog.Warn("transcript.mkdir_failed",
			"component", "transcript-router",
			"path", filepath.Dir(full),
			"err", err,
		)
		return nil, fmt.Errorf("transcript: mkdir %s: %w", filepath.Dir(full), err)
	}
	file, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, transcriptFilePerm)
	if err != nil {
		slog.Warn("transcript.open_failed",
			"component", "transcript-router",
			"path", full,
			"err", err,
		)
		return nil, fmt.Errorf("transcript: open %s: %w", full, err)
	}
	h := &transcriptHandle{path: full, file: file}
	s.cache[relPath] = h
	s.order = append(s.order, relPath)
	return h, nil
}

func (s *transcriptRouterState) touchLocked(relPath string) {
	for i, k := range s.order {
		if k == relPath {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.order = append(s.order, relPath)
}

func (s *transcriptRouterState) evictOldestLocked() error {
	if len(s.order) == 0 {
		return nil
	}
	victim := s.order[0]
	s.order = s.order[1:]
	h, ok := s.cache[victim]
	if !ok {
		return nil
	}
	delete(s.cache, victim)
	err := h.file.Close()
	if err != nil {
		slog.Warn("transcript.evict_close_failed",
			"component", "transcript-router",
			"path", h.path,
			"err", err,
		)
		return fmt.Errorf("transcript: close evicted %s: %w", h.path, err)
	}
	return nil
}

// sanitizeChatKey replaces any rune outside [A-Za-z0-9_-] with _. Empty
// inputs and inputs that sanitize to all underscores still produce a non-empty
// result by passing the underscored form through; the caller decides whether
// to reject. Path separators, dots, and slashes are normalized so a malicious
// chat_key like "../../etc/passwd" cannot escape the chats/ root.
func sanitizeChatKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	hasAccepted := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			hasAccepted = true
		default:
			b.WriteRune('_')
		}
	}
	if !hasAccepted {
		return ""
	}
	return b.String()
}

// transcriptRouterCloser bridges the router into the existing tee handler
// chain so Setup can return a single [io.Closer] that flushes both the file
// handlers and the router's open handle pool.
type transcriptRouterCloser struct {
	router *TranscriptRouter
	inner  io.Closer
}

func (c *transcriptRouterCloser) Close() error {
	var errs []error
	if c.inner != nil {
		err := c.inner.Close()
		if err != nil {
			slog.Warn("transcript.inner_close_failed",
				"component", "transcript-router",
				"err", err,
			)
			errs = append(errs, err)
		}
	}
	if c.router != nil {
		err := c.router.Close()
		if err != nil {
			slog.Warn("transcript.router_close_failed",
				"component", "transcript-router",
				"err", err,
			)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
