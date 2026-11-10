package slogger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog"
)

func TestCleanupCombinedBudgetPreservesActiveWriters(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	rotation := gklog.RotationConfig{MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 7}
	paths := []string{
		"clyde-daemon.jsonl", "clyde-cli.jsonl", "codex.jsonl", "anthropic.jsonl",
		"audit.jsonl", "logs/inventory/events.jsonl", "metrics/rollup.jsonl",
	}
	var handlers []slog.Handler
	var closers []io.Closer
	initial := make(map[string]os.FileInfo)
	payload := strings.Repeat("x", 450*1024)
	for _, relative := range paths {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		handler := gklog.FileJSON(path, slog.LevelInfo, rotation)
		handlers = append(handlers, handler)
		closers = append(closers, handler.(io.Closer))
		slog.New(handler).Info("initial", "payload", payload)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		initial[path] = info
	}
	routerRoot := filepath.Join(root, "logs", "concerns")
	router := gklog.NewRouter(routerRoot, slog.LevelInfo, slog.DiscardHandler, gklog.RouterOptions{Rotation: rotation})
	for index := range 5 {
		slog.New(router).Info(fmt.Sprintf("concern%d.initial", index), "payload", payload)
	}
	chatRoot := filepath.Join(root, "logs", "chats")
	transcripts := slogger.NewTranscriptRouter(slogger.TranscriptRouterConfig{Root: chatRoot})
	closers = append(closers, transcripts)
	slog.New(transcripts).Info("logging.request.leg", "chat_key", "chat", "payload", payload)
	t.Cleanup(func() {
		for _, closer := range closers {
			if err := closer.Close(); err != nil {
				t.Error(err)
			}
		}
	})

	result, err := slogger.RunCleanupOnce(slogger.CleanupPolicy{Enabled: true, Root: root, MaxTotalMB: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesBefore <= 5*1024*1024 || result.BytesAfter > 5*1024*1024 || result.BytesTrimmed <= 0 {
		t.Fatalf("cleanup did not enforce the combined budget: %+v", result)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
			return os.Chtimes(path, old, old)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := slogger.RunCleanupOnce(slogger.CleanupPolicy{Enabled: true, Root: root, MaxAgeDays: 1}); err != nil {
		t.Fatal(err)
	}
	for _, handler := range handlers {
		if err := handler.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "continued", 0)); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 5 {
		slog.New(router).Info(fmt.Sprintf("concern%d.continued", index))
	}
	slog.New(transcripts).Info("logging.request.leg", "chat_key", "chat", "continued", true)
	for path, before := range initial {
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("cleanup replaced the active inode %s: %v", path, err)
		}
		assertCompleteJSONLines(t, path)
	}
	for index := range 5 {
		assertCompleteJSONLines(t, filepath.Join(routerRoot, fmt.Sprintf("concern%d.jsonl", index)))
	}
	assertCompleteJSONLines(t, filepath.Join(chatRoot, "chat.jsonl"))
}

func TestCleanupExternalPathSelectsOnlyOwnedArchives(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	path := filepath.Join(external, "owned.jsonl")
	unrelated := filepath.Join(external, "unrelated.jsonl.1")
	files := []string{
		path,
		unrelated,
		filepath.Join(external, "owned.jsonl.1"),
		filepath.Join(external, "owned-2026-10-01T12-00-00.000.jsonl.gz"),
		filepath.Join(external, "owned-2026-10-02T12-00-00.000.jsonl.gz"),
	}
	for index, file := range files {
		if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		timestamp := time.Now().Add(time.Duration(index-10) * time.Hour)
		if err := os.Chtimes(file, timestamp, timestamp); err != nil {
			t.Fatal(err)
		}
	}
	result, err := slogger.RunCleanupOnce(slogger.CleanupPolicy{
		Enabled: true, Root: root, Paths: []string{path}, MaxBackups: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 2 || result.BytesDeleted != 6 || len(result.DeletedPaths) != 2 {
		t.Fatalf("unexpected archive cleanup: %+v", result)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("cleanup changed an unrelated external file: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cleanup removed the active external file: %v", err)
	}
}

func TestCleanupSupervisorPreservesOpenAppendDescriptor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "daemon.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	line := strings.Repeat("stderr ", 256) + "\n"
	if _, err := io.WriteString(file, strings.Repeat(line, 1024)); err != nil {
		t.Fatal(err)
	}
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	result, err := slogger.RunCleanupOnce(slogger.CleanupPolicy{Enabled: true, Root: root, MaxTotalMB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesAfter > 1024*1024 || len(result.TrimmedPaths) != 1 {
		t.Fatalf("supervisor cleanup exceeded its budget: %+v", result)
	}
	if _, err := io.WriteString(file, "continued\n"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("supervisor inode changed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("continued\n")) || bytes.IndexByte(data, 0) >= 0 {
		t.Fatal("supervisor descriptor did not append after compaction")
	}
}

func TestCleanupDiscardsPartialJSONLines(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.jsonl")
	data := bytes.Repeat([]byte("{\"value\":\"complete\"}\n"), 100000)
	data = append(data, []byte("{\"partial\":")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := slogger.RunCleanupOnce(slogger.CleanupPolicy{Enabled: true, Root: root, MaxTotalMB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesAfter > 1024*1024 || result.BytesBefore-result.BytesAfter != result.BytesTrimmed {
		t.Fatalf("incorrect byte accounting: %+v", result)
	}
	assertCompleteJSONLines(t, path)
}

func TestCleanupCompactsOpenArchiveWithoutUnlinking(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "clyde-daemon-2026-10-22T10-18-37.221.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(bytes.Repeat([]byte("{\"event\":\"initial\"}\n"), 100000)); err != nil {
		t.Fatal(err)
	}
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	result, err := slogger.RunCleanupOnce(slogger.CleanupPolicy{
		Enabled: true, Root: root, MaxTotalMB: 1, KeepInPlacePaths: []string{path},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 0 || result.Trimmed != 1 || result.BytesAfter > 1024*1024 {
		t.Fatalf("unexpected open archive result: %+v", result)
	}
	if _, err := io.WriteString(file, "{\"event\":\"continued\"}\n"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("cleanup changed the open archive inode: %v", err)
	}
	assertCompleteJSONLines(t, path)
}

func assertCompleteJSONLines(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatalf("expected a continued record in %s", path)
	}
	if data[len(data)-1] != '\n' {
		t.Fatalf("incomplete trailing record in %s", path)
	}
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatalf("invalid JSONL record in %s", path)
		}
	}
}

func TestCleanupReportsProtectedBytesWithoutChangingFile(t *testing.T) {
	root := t.TempDir()
	protected := filepath.Join(root, "supervisor.jsonl")
	ordinary := filepath.Join(root, "ordinary.jsonl")
	data := bytes.Repeat([]byte("{\"event\":\"protected\"}\n"), 100000)
	if err := os.WriteFile(protected, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ordinary, data, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := slogger.RunCleanupOnce(slogger.CleanupPolicy{
		Enabled: true, Root: root, MaxTotalMB: 1, ProtectedPaths: []string{protected},
	})
	if err == nil {
		t.Fatal("cleanup must report excess bytes when the protected file exceeds its budget")
	}
	if result.ProtectedBytes != int64(len(data)) || result.ExcessBytes != int64(len(data))-1024*1024 {
		t.Fatalf("incorrect protected byte accounting: %+v", result)
	}
	retained, err := os.ReadFile(protected)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retained, data) {
		t.Fatal("cleanup changed the protected file")
	}
	info, err := os.Stat(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("cleanup did not reclaim the unprotected file: size=%d", info.Size())
	}
}
