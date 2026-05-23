package slogger

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"goodkind.io/gklog"
)

const (
	inventoryIndexSinkName = "inventory_index"
	inventorySinkAttrName  = "sinks"
)

type inventoryFilterState struct {
	path            string
	level           slog.Level
	rotation        gklog.RotationConfig
	rotationEnabled bool
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
			path:            path,
			level:           level,
			rotation:        rotation,
			rotationEnabled: policy.Rotation.Enabled,
			closed:          atomic.Bool{},
		},
	}
}

func (h *inventoryFilterHandler) Close() error {
	if h == nil || h.state == nil {
		return nil
	}
	h.state.closed.Store(true)
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
	if err := os.MkdirAll(filepath.Dir(h.state.path), 0o755); err != nil {
		slog.Warn("slogger.inventory_index.mkdir_failed", "component", "slogger", "path", filepath.Dir(h.state.path), "err", err)
		return fmt.Errorf("create inventory index dir: %w", err)
	}
	lock, err := lockInventoryIndex(h.state.path)
	if err != nil {
		return err
	}
	defer unlockInventoryIndex(lock)
	if h.state.rotationEnabled {
		writer := gklog.NewLumberjackWriterWithConfig(h.state.path, h.state.rotation)
		defer func() { _ = writer.Close() }()
		if _, err := writer.Write(record); err != nil {
			slog.Warn("slogger.inventory_index.write_rotated_failed", "component", "slogger", "path", h.state.path, "err", err)
			return fmt.Errorf("write rotated inventory index event: %w", err)
		}
		return nil
	}
	file, err := os.OpenFile(h.state.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("slogger.inventory_index.open_failed", "component", "slogger", "path", h.state.path, "err", err)
		return fmt.Errorf("open inventory index: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(record); err != nil {
		slog.Warn("slogger.inventory_index.write_failed", "component", "slogger", "path", h.state.path, "err", err)
		return fmt.Errorf("write inventory index event: %w", err)
	}
	return nil
}

func lockInventoryIndex(path string) (*os.File, error) {
	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		slog.Warn("slogger.inventory_index.lock_open_failed", "component", "slogger", "path", lockPath, "err", err)
		return nil, fmt.Errorf("open inventory index lock: %w", err)
	}
	if err := flockInventoryFile(file, syscall.LOCK_EX); err != nil {
		_ = file.Close()
		slog.Warn("slogger.inventory_index.lock_failed", "component", "slogger", "path", lockPath, "err", err)
		return nil, fmt.Errorf("acquire inventory index lock: %w", err)
	}
	return file, nil
}

func unlockInventoryIndex(file *os.File) {
	if file == nil {
		return
	}
	_ = flockInventoryFile(file, syscall.LOCK_UN)
	_ = file.Close()
}

func flockInventoryFile(file *os.File, how int) error {
	fileDescriptor, err := strconv.Atoi(strconv.FormatUint(uint64(file.Fd()), 10))
	if err != nil {
		slog.Warn("slogger.inventory_index.lock_fd_failed", "component", "slogger", "err", err)
		return fmt.Errorf("convert inventory index lock file descriptor: %w", err)
	}
	if err := syscall.Flock(fileDescriptor, how); err != nil {
		slog.Warn("slogger.inventory_index.flock_failed", "component", "slogger", "err", err)
		return fmt.Errorf("flock inventory index lock file: %w", err)
	}
	return nil
}

func attrCarriesInventorySink(attr slog.Attr) bool {
	return attrCarriesSinkName(attr, inventoryIndexSinkName)
}
