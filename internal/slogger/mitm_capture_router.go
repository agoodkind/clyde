package slogger

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"goodkind.io/gklog"
)

const mitmCaptureIndexSinkName = "mitm_capture_index"

// MITMCapturePolicy describes the MITM capture index sink.
type MITMCapturePolicy struct {
	Enabled  bool
	Root     string
	Rotation RotationPolicy
}

type mitmCaptureFilterHandler struct {
	attrs           []slog.Attr
	groups          []string
	path            string
	level           slog.Level
	rotation        gklog.RotationConfig
	rotationEnabled bool
}

// NewMITMCaptureIndexHandler returns a handler that writes only events marked
// for the mitm_capture_index sink.
func NewMITMCaptureIndexHandler(policy MITMCapturePolicy, level slog.Level) slog.Handler {
	if !policy.Enabled {
		return nil
	}
	root := strings.TrimSpace(policy.Root)
	if root == "" {
		return nil
	}
	path := filepath.Join(root, "capture.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("slogger.mitm_capture_index.mkdir_failed", "component", "slogger", "path", filepath.Dir(path), "err", err)
		return nil
	}
	rotation := gklog.RotationConfig{}
	if policy.Rotation.Enabled {
		rotation = rotationConfig(policy.Rotation)
	}
	return &mitmCaptureFilterHandler{
		attrs:           nil,
		groups:          nil,
		path:            path,
		level:           level,
		rotation:        rotation,
		rotationEnabled: policy.Rotation.Enabled,
	}
}

func buildMITMCaptureIndexHandler(policy MITMCapturePolicy, level slog.Level) slog.Handler {
	return NewMITMCaptureIndexHandler(policy, level)
}

func (h *mitmCaptureFilterHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *mitmCaptureFilterHandler) Handle(ctx context.Context, record slog.Record) error {
	if !h.matches(record) {
		return nil
	}
	encodedRecord, err := h.encode(ctx, record)
	if err != nil {
		return err
	}
	return h.write(encodedRecord)
}

func (h *mitmCaptureFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &mitmCaptureFilterHandler{
		attrs:           append(append([]slog.Attr(nil), h.attrs...), attrs...),
		groups:          append([]string(nil), h.groups...),
		path:            h.path,
		level:           h.level,
		rotation:        h.rotation,
		rotationEnabled: h.rotationEnabled,
	}
}

func (h *mitmCaptureFilterHandler) WithGroup(name string) slog.Handler {
	return &mitmCaptureFilterHandler{
		attrs:           append([]slog.Attr(nil), h.attrs...),
		groups:          append(append([]string(nil), h.groups...), name),
		path:            h.path,
		level:           h.level,
		rotation:        h.rotation,
		rotationEnabled: h.rotationEnabled,
	}
}

func attrCarriesSinkName(attr slog.Attr, sinkName string) bool {
	if attr.Key != inventorySinkAttrName {
		return false
	}
	return strings.Contains(attr.Value.String(), sinkName)
}

func (h *mitmCaptureFilterHandler) matches(record slog.Record) bool {
	for _, attr := range h.attrs {
		if attrCarriesSinkName(attr, mitmCaptureIndexSinkName) {
			return true
		}
	}
	matched := false
	record.Attrs(func(attr slog.Attr) bool {
		if attrCarriesSinkName(attr, mitmCaptureIndexSinkName) {
			matched = true
			return false
		}
		return true
	})
	return matched
}

func (h *mitmCaptureFilterHandler) encode(ctx context.Context, record slog.Record) ([]byte, error) {
	var buffer bytes.Buffer
	var handler slog.Handler = slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: h.level})
	for _, group := range h.groups {
		handler = handler.WithGroup(group)
	}
	if len(h.attrs) > 0 {
		handler = handler.WithAttrs(h.attrs)
	}
	if err := handler.Handle(ctx, record); err != nil {
		slog.WarnContext(ctx, "slogger.mitm_capture_index.encode_failed", "component", "slogger", "path", h.path, "err", err)
		return nil, fmt.Errorf("encode MITM capture index event: %w", err)
	}
	return buffer.Bytes(), nil
}

func (h *mitmCaptureFilterHandler) write(record []byte) error {
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		slog.Warn("slogger.mitm_capture_index.mkdir_failed", "component", "slogger", "path", filepath.Dir(h.path), "err", err)
		return fmt.Errorf("create MITM capture index dir: %w", err)
	}
	lock, err := lockMITMCaptureIndex(h.path)
	if err != nil {
		return err
	}
	defer unlockMITMCaptureIndex(lock)
	if h.rotationEnabled {
		writer := gklog.NewLumberjackWriterWithConfig(h.path, h.rotation)
		defer func() { _ = writer.Close() }()
		if _, err := writer.Write(record); err != nil {
			slog.Warn("slogger.mitm_capture_index.write_rotated_failed", "component", "slogger", "path", h.path, "err", err)
			return fmt.Errorf("write rotated MITM capture index event: %w", err)
		}
		return nil
	}
	file, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("slogger.mitm_capture_index.open_failed", "component", "slogger", "path", h.path, "err", err)
		return fmt.Errorf("open MITM capture index: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(record); err != nil {
		slog.Warn("slogger.mitm_capture_index.write_failed", "component", "slogger", "path", h.path, "err", err)
		return fmt.Errorf("write MITM capture index event: %w", err)
	}
	return nil
}

func lockMITMCaptureIndex(path string) (*os.File, error) {
	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		slog.Warn("slogger.mitm_capture_index.lock_open_failed", "component", "slogger", "path", lockPath, "err", err)
		return nil, fmt.Errorf("open MITM capture index lock: %w", err)
	}
	if err := flockMITMCaptureFile(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		slog.Warn("slogger.mitm_capture_index.lock_failed", "component", "slogger", "path", lockPath, "err", err)
		return nil, fmt.Errorf("acquire MITM capture index lock: %w", err)
	}
	return file, nil
}

func unlockMITMCaptureIndex(file *os.File) {
	if file == nil {
		return
	}
	_ = flockMITMCaptureFile(file, syscall.LOCK_UN)
	_ = file.Close()
}

func flockMITMCaptureFile(file *os.File, how int) error {
	fileDescriptor, err := strconv.Atoi(strconv.FormatUint(uint64(file.Fd()), 10))
	if err != nil {
		slog.Warn("slogger.mitm_capture_index.lock_fd_failed", "component", "slogger", "err", err)
		return fmt.Errorf("convert MITM capture lock file descriptor: %w", err)
	}
	if err := syscall.Flock(fileDescriptor, how); err != nil {
		slog.Warn("slogger.mitm_capture_index.flock_failed", "component", "slogger", "err", err)
		return fmt.Errorf("flock MITM capture lock file: %w", err)
	}
	return nil
}
