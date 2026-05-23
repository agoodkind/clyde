package slogger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/gklog"
)

const (
	inventoryIndexSinkName = "inventory_index"
	inventorySinkAttrName  = "sinks"
)

type inventoryFilterHandler struct {
	attrs   []slog.Attr
	handler slog.Handler
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
	return newInventoryFilterHandler(gklog.FileJSON(path, level, rotation))
}

func newInventoryFilterHandler(handler slog.Handler) slog.Handler {
	return &inventoryFilterHandler{attrs: nil, handler: handler}
}

func (h *inventoryFilterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *inventoryFilterHandler) Handle(ctx context.Context, record slog.Record) error {
	if !h.matches(record) {
		return nil
	}
	if err := h.handler.Handle(ctx, record); err != nil {
		slog.WarnContext(ctx, "slogger.inventory_index.write_failed", "component", "slogger", "err", err)
		return fmt.Errorf("write inventory index event: %w", err)
	}
	return nil
}

func (h *inventoryFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &inventoryFilterHandler{
		attrs:   append(append([]slog.Attr(nil), h.attrs...), attrs...),
		handler: h.handler.WithAttrs(attrs),
	}
}

func (h *inventoryFilterHandler) WithGroup(name string) slog.Handler {
	return &inventoryFilterHandler{
		attrs:   append([]slog.Attr(nil), h.attrs...),
		handler: h.handler.WithGroup(name),
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

func attrCarriesInventorySink(attr slog.Attr) bool {
	return attrCarriesSinkName(attr, inventoryIndexSinkName)
}
