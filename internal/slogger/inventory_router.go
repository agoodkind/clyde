package slogger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"goodkind.io/gklog"
)

const (
	inventoryIndexSinkName = "inventory_index"
	inventorySinkAttrName  = "sinks"
)

type inventoryWriterFactory func(path string, rotation gklog.RotationConfig, rotationEnabled bool) (io.WriteCloser, error)

type inventoryFilterState struct {
	mu              sync.Mutex
	path            string
	level           slog.Level
	rotation        gklog.RotationConfig
	rotationEnabled bool
	writer          io.WriteCloser
	writerFactory   inventoryWriterFactory
	closed          atomic.Bool
}

type inventoryFilterHandler struct {
	attrs  []slog.Attr
	groups []string
	state  *inventoryFilterState
}

func buildInventoryIndexHandler(policy InventoryPolicy, concernRoot string, level slog.Level) slog.Handler {
	if !policy.Enabled {
		return nil
	}
	root := strings.TrimSpace(policy.Root)
	if root == "" {
		root = filepath.Join(concernRoot, "inventory")
	}
	path := filepath.Join(root, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("slogger.inventory_index.mkdir_failed", "component", "slogger", "path", filepath.Dir(path), "err", err)
		return nil
	}
	rotation := gklog.RotationConfig{}
	if policy.Rotation.Enabled {
		rotation = rotationConfig(policy.Rotation)
	}
	return &inventoryFilterHandler{
		attrs:  nil,
		groups: nil,
		state: &inventoryFilterState{
			mu:              sync.Mutex{},
			path:            path,
			level:           level,
			rotation:        rotation,
			rotationEnabled: policy.Rotation.Enabled,
			writer:          nil,
			writerFactory:   newInventoryIndexWriter,
			closed:          atomic.Bool{},
		},
	}
}

func (h *inventoryFilterHandler) Close() error {
	if h == nil || h.state == nil {
		return nil
	}
	if h.state.closed.Swap(true) {
		return nil
	}
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.state.writer == nil {
		return nil
	}
	err := h.state.writer.Close()
	h.state.writer = nil
	if err != nil {
		slog.Warn("slogger.inventory_index.close_failed", "component", "slogger", "path", h.state.path, "err", err)
		return fmt.Errorf("close inventory index writer: %w", err)
	}
	return nil
}

func (h *inventoryFilterHandler) Enabled(_ context.Context, level slog.Level) bool {
	if h == nil || h.state == nil || h.state.closed.Load() {
		return false
	}
	return level >= h.state.level
}

func (h *inventoryFilterHandler) Handle(ctx context.Context, record slog.Record) error {
	if h == nil || h.state == nil || h.state.closed.Load() {
		return nil
	}
	if !h.matches(record) {
		return nil
	}
	encodedRecord, err := h.encode(ctx, record)
	if err != nil {
		return err
	}
	return h.write(encodedRecord)
}

func (h *inventoryFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &inventoryFilterHandler{
		attrs:  append(append([]slog.Attr(nil), h.attrs...), attrs...),
		groups: append([]string(nil), h.groups...),
		state:  h.state,
	}
}

func (h *inventoryFilterHandler) WithGroup(name string) slog.Handler {
	return &inventoryFilterHandler{
		attrs:  append([]slog.Attr(nil), h.attrs...),
		groups: append(append([]string(nil), h.groups...), name),
		state:  h.state,
	}
}

func (h *inventoryFilterHandler) matches(record slog.Record) bool {
	if slices.ContainsFunc(h.attrs, attrCarriesInventorySink) {
		return true
	}
	matched := false
	record.Attrs(func(attr slog.Attr) bool {
		if attrCarriesInventorySink(attr) {
			matched = true
			return false
		}
		return true
	})
	return matched
}

func (h *inventoryFilterHandler) encode(ctx context.Context, record slog.Record) ([]byte, error) {
	attrs := make([]slog.Attr, 0, len(h.attrs)+16)
	attrs = append(attrs, h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	attrs = dedupeInventoryAttrs(attrs)

	var buffer bytes.Buffer
	var handler slog.Handler = slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: h.state.level})
	for _, group := range h.groups {
		handler = handler.WithGroup(group)
	}
	if len(attrs) > 0 {
		handler = handler.WithAttrs(attrs)
	}
	cleanRecord := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	if err := handler.Handle(ctx, cleanRecord); err != nil {
		slog.WarnContext(ctx, "slogger.inventory_index.encode_failed", "component", "slogger", "path", h.state.path, "err", err)
		return nil, fmt.Errorf("encode inventory index event: %w", err)
	}
	return buffer.Bytes(), nil
}

func dedupeInventoryAttrs(attrs []slog.Attr) []slog.Attr {
	lastIndex := make(map[string]int)
	for index, attr := range attrs {
		if !shouldDedupeInventoryAttr(attr.Key) {
			continue
		}
		lastIndex[attr.Key] = index
	}
	out := make([]slog.Attr, 0, len(attrs))
	for index, attr := range attrs {
		if shouldDedupeInventoryAttr(attr.Key) && lastIndex[attr.Key] != index {
			continue
		}
		out = append(out, attr)
	}
	return out
}

func shouldDedupeInventoryAttr(key string) bool {
	return key == "component" || key == "concern" || key == "request_id"
}

func (h *inventoryFilterHandler) write(record []byte) error {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.state.closed.Load() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(h.state.path), 0o755); err != nil {
		slog.Warn("slogger.inventory_index.mkdir_failed", "component", "slogger", "path", filepath.Dir(h.state.path), "err", err)
		return fmt.Errorf("create inventory index dir: %w", err)
	}
	if h.state.writer == nil {
		writer, err := h.state.writerFactory(h.state.path, h.state.rotation, h.state.rotationEnabled)
		if err != nil {
			slog.Warn("slogger.inventory_index.open_failed", "component", "slogger", "path", h.state.path, "err", err)
			return fmt.Errorf("open inventory index writer: %w", err)
		}
		h.state.writer = writer
	}
	if _, err := h.state.writer.Write(record); err != nil {
		slog.Warn("slogger.inventory_index.write_failed", "component", "slogger", "path", h.state.path, "err", err)
		return fmt.Errorf("write inventory index event: %w", err)
	}
	return nil
}

func attrCarriesInventorySink(attr slog.Attr) bool {
	if attr.Key != inventorySinkAttrName {
		return false
	}
	return strings.Contains(attr.Value.String(), inventoryIndexSinkName)
}

func newInventoryIndexWriter(path string, rotation gklog.RotationConfig, rotationEnabled bool) (io.WriteCloser, error) {
	if rotationEnabled {
		writer := gklog.NewLockedWriteCloser(path, gklog.NewLumberjackWriterWithConfig(path, rotation))
		return requireInventoryIndexWriter(path, writer)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("slogger.inventory_index.open_failed", "component", "slogger", "path", path, "err", err)
		return nil, fmt.Errorf("open inventory index: %w", err)
	}
	return requireInventoryIndexWriter(path, gklog.NewLockedWriteCloser(path, file))
}

func requireInventoryIndexWriter(path string, writer io.WriteCloser) (io.WriteCloser, error) {
	if writer == nil {
		return nil, fmt.Errorf("inventory index writer is nil for %s", path)
	}
	return writer, nil
}
