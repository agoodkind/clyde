// Package slogger transcript_router routes records that carry a chat_key
// attribute into chats/<chat_key>/<YYYY-MM-DD>/<request_id>.jsonl files
// alongside the existing concern handlers. Only an explicit allowlist of msg
// names is teed; everything else is unaffected.
package slogger

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
	// Root is the directory under which chats/<chat_key>/<date>/ files
	// are written. Typically <state>/clyde/logs/chats.
	Root string
	// Mode is summary or raw.
	Mode TranscriptMode
	// PoolCap caps simultaneously open file handles. Zero falls back to
	// transcriptDefaultPoolCap.
	PoolCap int
	// Now is injectable for tests. Defaults to time.Now.
	Now func() time.Time
}

// TranscriptRouter is an slog.Handler that tees allowlisted records into
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
// implements slog.Handler and is safe for concurrent use.
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

// Enabled implements slog.Handler. The router is unconditionally enabled at
// the level the caller produced; the allowlist filters by msg name later.
func (r *TranscriptRouter) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

// Handle implements slog.Handler. Non-allowlisted msgs and records without a
// resolvable chat_key are dropped without error. Write failures are returned
// so the upstream tee handler can surface them.
func (r *TranscriptRouter) Handle(ctx context.Context, record slog.Record) error {
	if _, ok := transcriptAllowlist[record.Message]; !ok {
		return nil
	}
	chatKey, requestID := r.extractKeys(record)
	if chatKey == "" {
		return nil
	}
	sanitized := sanitizeChatKey(chatKey)
	if sanitized == "" {
		return nil
	}
	if requestID == "" {
		requestID = "no-request-id"
	}
	requestID = sanitizeChatKey(requestID)

	date := r.state.cfg.Now().UTC().Format("2006-01-02")
	relPath := filepath.Join(sanitized, date, requestID+".jsonl")

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
	if _, err := handle.file.Write(payload); err != nil {
		slog.WarnContext(ctx, "transcript.write_failed",
			"component", "transcript-router",
			"err", err,
			"path", handle.path,
		)
		return fmt.Errorf("transcript: write %s: %w", handle.path, err)
	}
	return nil
}

// WithAttrs implements slog.Handler. The clone shares the underlying LRU pool
// state so all derived loggers route into the same per-chat files.
func (r *TranscriptRouter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TranscriptRouter{
		state: r.state,
		attrs: append(append([]slog.Attr(nil), r.attrs...), attrs...),
		group: r.group,
	}
}

// WithGroup implements slog.Handler. Groups are tracked but not used for
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
		if err := h.file.Close(); err != nil {
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
		return nil, err
	}
	out["time"] = tsBytes
	lvlBytes, err := json.Marshal(record.Level.String())
	if err != nil {
		return nil, err
	}
	out["level"] = lvlBytes
	msgBytes, err := json.Marshal(record.Message)
	if err != nil {
		return nil, err
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
			return err
		}
		out[attr.Key] = raw
		return nil
	}
	for _, attr := range r.attrs {
		if attr.Key == "" {
			continue
		}
		if err := addAttr(attr); err != nil {
			return nil, err
		}
	}
	var attrErr error
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "" {
			return true
		}
		if err := addAttr(attr); err != nil {
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
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func (s *transcriptRouterState) acquireLocked(relPath string) (*transcriptHandle, error) {
	if h, ok := s.cache[relPath]; ok {
		s.touchLocked(relPath)
		return h, nil
	}
	if len(s.cache) >= s.cfg.PoolCap {
		if err := s.evictOldestLocked(); err != nil {
			return nil, err
		}
	}
	full := filepath.Join(s.cfg.Root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), transcriptDirPerm); err != nil {
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
	if err := h.file.Close(); err != nil {
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
	any := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			any = true
		default:
			b.WriteRune('_')
		}
	}
	if !any {
		return ""
	}
	return b.String()
}

// transcriptRouterCloser bridges the router into the existing tee handler
// chain so Setup can return a single io.Closer that flushes both the file
// handlers and the router's open handle pool.
type transcriptRouterCloser struct {
	router *TranscriptRouter
	inner  io.Closer
}

func (c *transcriptRouterCloser) Close() error {
	var errs []error
	if c.inner != nil {
		if err := c.inner.Close(); err != nil {
			slog.Warn("transcript.inner_close_failed",
				"component", "transcript-router",
				"err", err,
			)
			errs = append(errs, err)
		}
	}
	if c.router != nil {
		if err := c.router.Close(); err != nil {
			slog.Warn("transcript.router_close_failed",
				"component", "transcript-router",
				"err", err,
			)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
