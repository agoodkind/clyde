package slogger

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/gklog/correlation"
)

func TestDefaultProcessPathUsesConfigStateResolver(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "~/state/../state-root")
	t.Setenv("CLYDE_SLOG_PATH", "")

	got := DefaultProcessPath(config.LoggingConfig{}, ProcessRoleDaemon)
	want := filepath.Join(home, "state-root", "clyde", "clyde-daemon.jsonl")
	if got != want {
		t.Fatalf("DefaultProcessPath()=%q want %q", got, want)
	}
}

func TestSetupWritesConcernLogToHardCodedNestedTree(t *testing.T) {
	root := t.TempDir()
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	policy := testSetupPolicy(root)
	policy.Level = slog.LevelDebug
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	For(ConcernAdapterModelsCatalog).Info("adapter.models.listed", "model_count", 42)
	slog.Info("unconcerned.event")
	_ = closer.Close()

	modelsPath := filepath.Join(root, "logs", "adapter", "models", "catalog.jsonl")
	modelsLog, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatalf("read concern log %s: %v", modelsPath, err)
	}
	if !strings.Contains(string(modelsLog), `"msg":"adapter.models.listed"`) {
		t.Fatalf("concern log missing models event: %s", modelsLog)
	}
	if strings.Contains(string(modelsLog), "unconcerned.event") {
		t.Fatalf("concern log should not include unconcerned event: %s", modelsLog)
	}

	unifiedLog, err := os.ReadFile(unified)
	if err != nil {
		t.Fatalf("read unified log: %v", err)
	}
	if !strings.Contains(string(unifiedLog), "adapter.models.listed") || !strings.Contains(string(unifiedLog), "unconcerned.event") {
		t.Fatalf("unified log should keep both events: %s", unifiedLog)
	}
}

func TestSetupRoutesExistingEventNamesWhenConcernIsExplicit(t *testing.T) {
	root := t.TempDir()
	policy := testSetupPolicy(root)
	policy.Level = slog.LevelDebug
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	For(ConcernMCPServerContext).Warn("mcp.context.load_failed", "path", "/tmp/nope")
	For(ConcernDaemonWorkersReload).Info("daemon.workers.reload.started", "reason", "test")
	For(ConcernMCPServerRequest).Debug("mcp.requests.started", "tool", "needle")
	_ = closer.Close()

	assertLogContains(t, filepath.Join(root, "logs", "mcp", "server", "context.jsonl"), "mcp.context.load_failed")
	assertLogContains(t, filepath.Join(root, "logs", "daemon", "workers", "reload.jsonl"), "daemon.workers.reload.started")
	assertLogContains(t, filepath.Join(root, "logs", "mcp", "server", "requests.jsonl"), "mcp.requests.started")
}

func TestConcernLoggerResolvesDefaultAfterSetup(t *testing.T) {
	root := t.TempDir()
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	early := Concern(ConcernMCPServerContext)

	policy := testSetupPolicy(root)
	policy.Level = slog.LevelDebug
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	early.Logger().Info("mcp.context.lazy_logger", "conversation_id", "demo")
	_ = closer.Close()

	unifiedLog, err := os.ReadFile(unified)
	if err != nil {
		t.Fatalf("read unified log: %v", err)
	}
	if strings.Contains(string(unifiedLog), `"msg":"INFO mcp.context.lazy_logger`) {
		t.Fatalf("lazy concern logger used bootstrap text logger: %s", unifiedLog)
	}
	if !strings.Contains(string(unifiedLog), `"msg":"mcp.context.lazy_logger"`) {
		t.Fatalf("unified log missing lazy logger event: %s", unifiedLog)
	}
	assertLogContains(t, filepath.Join(root, "logs", "mcp", "server", "context.jsonl"), "mcp.context.lazy_logger")
}

func TestSetupInjectsContextCorrelationAttrs(t *testing.T) {
	root := t.TempDir()
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	policy := testSetupPolicy(root)
	policy.Level = slog.LevelDebug
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	corr := correlation.Context{
		TraceID:      "0123456789abcdef0123456789abcdef",
		SpanID:       "0123456789abcdef",
		ParentSpanID: "fedcba9876543210",
		RequestID:    "req-ctx",
		IdentityAttributes: []correlation.IdentityAttribute{
			{Key: "cursor_request_id", Value: "cursor-req"},
			{Key: "cursor_conversation_id", Value: "cursor-conv"},
		},
	}
	ctx := correlation.WithContext(context.Background(), corr)
	slog.InfoContext(ctx, "daemon.rpc.started", "request_id", "explicit-req")
	_ = closer.Close()

	event := readSingleEvent(t, unified)
	if event.Message != "daemon.rpc.started" {
		t.Fatalf("message = %q", event.Message)
	}
	if event.RequestID != "explicit-req" {
		t.Fatalf("request_id = %q, want explicit-req", event.RequestID)
	}
	if event.TraceID != string(corr.TraceID) {
		t.Fatalf("trace_id = %q, want %q", event.TraceID, corr.TraceID)
	}
	if event.SpanID != string(corr.SpanID) {
		t.Fatalf("span_id = %q, want %q", event.SpanID, corr.SpanID)
	}
	if event.ParentSpanID != string(corr.ParentSpanID) {
		t.Fatalf("parent_span_id = %q, want %q", event.ParentSpanID, corr.ParentSpanID)
	}
	if event.CursorRequestID != "cursor-req" {
		t.Fatalf("cursor_request_id = %q, want cursor-req", event.CursorRequestID)
	}
	if event.CursorConversationID != "cursor-conv" {
		t.Fatalf("cursor_conversation_id = %q, want cursor-conv", event.CursorConversationID)
	}
}

func TestSetupInjectsCorrelationAttrsIntoConcernLogWithoutOverwritingExplicitAttrs(t *testing.T) {
	root := t.TempDir()
	policy := testSetupPolicy(root)
	policy.Level = slog.LevelDebug
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	corr := correlation.Context{
		TraceID:      "0123456789abcdef0123456789abcdef",
		SpanID:       "0123456789abcdef",
		ParentSpanID: "fedcba9876543210",
		RequestID:    "req-ctx",
		IdentityAttributes: []correlation.IdentityAttribute{
			{Key: "cursor_generation_id", Value: "cursor-gen"},
		},
	}
	corr = clydeingress.WithUpstreamRequestID(corr, "upstream-req")
	corr = clydeingress.WithUpstreamResponseID(corr, "upstream-resp")
	ctx := correlation.WithContext(context.Background(), corr)
	For(ConcernDaemonWorkersReload).InfoContext(ctx,
		"daemon.workers.reload.started",
		"trace_id", "explicit-trace",
		"span_id", "explicit-span",
	)
	_ = closer.Close()

	event := readSingleEvent(t, filepath.Join(root, "logs", "daemon", "workers", "reload.jsonl"))
	if event.Message != "daemon.workers.reload.started" {
		t.Fatalf("message = %q", event.Message)
	}
	if event.TraceID != "explicit-trace" {
		t.Fatalf("trace_id = %q, want explicit-trace", event.TraceID)
	}
	if event.SpanID != "explicit-span" {
		t.Fatalf("span_id = %q, want explicit-span", event.SpanID)
	}
	if event.ParentSpanID != string(corr.ParentSpanID) {
		t.Fatalf("parent_span_id = %q, want %q", event.ParentSpanID, corr.ParentSpanID)
	}
	if event.RequestID != corr.RequestID {
		t.Fatalf("request_id = %q, want %q", event.RequestID, corr.RequestID)
	}
	if event.CursorGenerationID != "cursor-gen" {
		t.Fatalf("cursor_generation_id = %q, want cursor-gen", event.CursorGenerationID)
	}
	if event.UpstreamRequestID != "upstream-req" {
		t.Fatalf("upstream_request_id = %q, want upstream-req", event.UpstreamRequestID)
	}
	if event.UpstreamResponseID != "upstream-resp" {
		t.Fatalf("upstream_response_id = %q, want upstream-resp", event.UpstreamResponseID)
	}
}

func TestSetupWithPolicyAppliesPerConcernLevel(t *testing.T) {
	root := t.TempDir()
	policy := testSetupPolicy(root)
	debugLevel := slog.LevelDebug
	policy.ConcernPolicies = map[string]ConcernPolicy{
		ConcernAdapterModelsCatalog: {
			Enabled:  nil,
			Level:    &debugLevel,
			Rotation: nil,
		},
	}

	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	For(ConcernAdapterModelsCatalog).Debug("adapter.models.debug_event")
	For(ConcernDaemonWorkersReload).Debug("daemon.workers.reload.debug_event")
	_ = closer.Close()

	assertLogContains(t, filepath.Join(root, "logs", "adapter", "models", "catalog.jsonl"), "adapter.models.debug_event")
	assertLogMissing(t, filepath.Join(root, "logs", "daemon", "workers", "reload.jsonl"), "daemon.workers.reload.debug_event")
	assertLogMissing(t, filepath.Join(root, "clyde-daemon.jsonl"), "adapter.models.debug_event")
}

// TestSetupWithPolicyAppliesSharedRotationToConcernFiles proves the gklog
// router rotates per-concern files using the single shared rotation budget the
// policy carries. The router does not honor a per-concern rotation override; it
// applies one RotationConfig to every concern file it opens.
func TestSetupWithPolicyAppliesSharedRotationToConcernFiles(t *testing.T) {
	root := t.TempDir()
	policy := testSetupPolicy(root)
	policy.ProcessSink.Rotation = RotationPolicy{
		Enabled:    true,
		MaxSizeMB:  1,
		MaxBackups: 4,
		MaxAgeDays: 1,
		Compress:   new(false),
	}

	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	payload := strings.Repeat("x", 8192)
	for i := range 300 {
		For(ConcernAdapterModelsCatalog).Info("adapter.models.large_event", "index", i, "payload", payload)
	}
	_ = closer.Close()

	concernLogs, err := filepath.Glob(filepath.Join(root, "logs", "adapter", "models", "catalog*.jsonl*"))
	if err != nil {
		t.Fatalf("glob concern logs: %v", err)
	}
	concernLogs = filterLockFiles(concernLogs)
	if len(concernLogs) < 2 {
		t.Fatalf("shared concern rotation did not create backups: %v", concernLogs)
	}
}

// TestSetupWithPolicyKeepsConcernFilesUnrotatedWhenRotationDisabled proves a
// disabled shared rotation leaves one concern file with no rotated backups.
func TestSetupWithPolicyKeepsConcernFilesUnrotatedWhenRotationDisabled(t *testing.T) {
	root := t.TempDir()
	policy := testSetupPolicy(root)
	policy.ProcessSink.Rotation = RotationPolicy{
		Enabled:    false,
		MaxSizeMB:  0,
		MaxBackups: 0,
		MaxAgeDays: 0,
		Compress:   nil,
	}

	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	payload := strings.Repeat("x", 8192)
	for i := range 300 {
		For(ConcernAdapterModelsCatalog).Info("adapter.models.large_event", "index", i, "payload", payload)
	}
	_ = closer.Close()

	concernLogs, err := filepath.Glob(filepath.Join(root, "logs", "adapter", "models", "catalog*.jsonl*"))
	if err != nil {
		t.Fatalf("glob concern logs: %v", err)
	}
	concernLogs = filterLockFiles(concernLogs)
	if len(concernLogs) != 1 {
		t.Fatalf("disabled rotation should keep a single concern file, got: %v", concernLogs)
	}
}

func TestConcernRelPathMapsDottedConcernToNestedPath(t *testing.T) {
	cases := map[string]string{
		ConcernAdapterModelsCatalog: "adapter/models/catalog.jsonl",
		ConcernCLIMITMTruststore:    "cli/mitm/truststore.jsonl",
		ConcernConfig:               "config.jsonl",
		"":                          "",
		"   ":                       "",
	}
	for concern, want := range cases {
		if got := ConcernRelPath(concern); got != want {
			t.Fatalf("ConcernRelPath(%q) = %q, want %q", concern, got, want)
		}
	}
}

func TestSetupWithPolicyPreservesDefaultLevelAndTranscriptSummary(t *testing.T) {
	root := t.TempDir()
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	policy := testSetupPolicy(root)
	policy.TranscriptPolicy.Enabled = true
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	slog.Debug("daemon.debug_default_filtered")
	slog.Info("daemon.info_default_kept")
	slog.Info("logging.request.leg", "chat_key", "chat-defaults", "request_id", "req-defaults", "body", "secret-body")
	_ = closer.Close()

	assertLogContains(t, unified, "daemon.info_default_kept")
	assertLogMissing(t, unified, "daemon.debug_default_filtered")
	transcriptPath := filepath.Join(root, "logs", "chats", "chat-defaults.jsonl")
	assertLogContains(t, transcriptPath, "logging.request.leg")
	assertLogMissing(t, transcriptPath, "secret-body")
}

func TestSetupWithPolicyWritesInventoryIndexSink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	policy := testSetupPolicy(root)
	policy.InventoryPolicy.Enabled = true
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	slog.Info("logging.request.leg", "request_id", "req-inventory", "sinks", []logevent.SinkName{logevent.SinkProcess, logevent.SinkInventory})
	slog.Info("daemon.info_without_inventory", "request_id", "req-skip")
	_ = closer.Close()

	inventoryPath := filepath.Join(root, "logs", "inventory", "events.jsonl")
	assertLogContains(t, inventoryPath, "req-inventory")
	assertLogMissing(t, inventoryPath, "req-skip")
}

func TestInventoryIndexHandlerWritesValidJSONLUnderConcurrentOverlap(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	policy := InventoryPolicy{
		Enabled: true,
		Root:    filepath.Join(root, "logs", "inventory"),
		Rotation: RotationPolicy{
			Enabled:    false,
			MaxSizeMB:  0,
			MaxBackups: 0,
			MaxAgeDays: 0,
			Compress:   nil,
		},
	}
	h1 := buildInventoryIndexHandler(policy, filepath.Join(root, "logs"), slog.LevelInfo)
	h2 := buildInventoryIndexHandler(policy, filepath.Join(root, "logs"), slog.LevelInfo)
	logger1 := WithConcern(slog.New(h1).With("component", "daemon"), ConcernProcessDaemonLifecycle)
	logger1 = WithConcern(logger1, ConcernProviderMITMLifecycle)
	logger1 = WithConcern(logger1, ConcernProviderMITMWire)
	logger2 := WithConcern(slog.New(h2).With("component", "daemon"), ConcernProcessDaemonLifecycle)
	logger2 = WithConcern(logger2, ConcernProviderMITMWire)

	var wg sync.WaitGroup
	const writers = 8
	const perWriter = 50
	for i := 0; i < writers; i++ {
		wg.Add(2)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				logger1.Info(
					"logging.request.leg",
					"component", "mitm",
					"request_id", fmt.Sprintf("req-a-%d-%d", workerID, j),
					"provider", "cursor",
					"surface", string(logevent.SurfaceMITMIDE),
					"leg", string(logevent.LegMITMForward),
					"sinks", []logevent.SinkName{logevent.SinkProcess, logevent.SinkInventory},
				)
			}
		}(i)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				logger2.Info(
					"logging.request.leg",
					"component", "mitm",
					"request_id", fmt.Sprintf("req-b-%d-%d", workerID, j),
					"provider", "claude",
					"surface", string(logevent.SurfaceMITMIDE),
					"leg", string(logevent.LegMITMCaptureIndex),
					"sinks", []logevent.SinkName{logevent.SinkConcern, logevent.SinkInventory},
				)
			}
		}(i)
	}
	wg.Wait()

	inventoryPath := filepath.Join(root, "logs", "inventory", "events.jsonl")
	content, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatalf("read inventory index: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	wantLines := writers * perWriter * 2
	if len(lines) != wantLines {
		t.Fatalf("inventory line count = %d, want %d", len(lines), wantLines)
	}
	for index, line := range lines {
		if !strings.HasPrefix(line, "{") {
			t.Fatalf("line %d does not start with '{': %q", index+1, line)
		}
		if strings.Count(line, `"concern":`) > 1 {
			t.Fatalf("line %d has duplicated concern attrs: %s", index+1, line)
		}
		if strings.Count(line, `"component":`) > 1 {
			t.Fatalf("line %d has duplicated component attrs: %s", index+1, line)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d invalid json: %v\n%s", index+1, err, line)
		}
	}
}

func assertLogContains(t *testing.T, path string, needle string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log %s: %v", path, err)
	}
	if !strings.Contains(string(content), needle) {
		t.Fatalf("log %s missing %q: %s", path, needle, content)
	}
}

func assertLogMissing(t *testing.T, path string, needle string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read log %s: %v", path, err)
	}
	if strings.Contains(string(content), needle) {
		t.Fatalf("log %s should not contain %q: %s", path, needle, content)
	}
}

func testSetupPolicy(root string) SetupPolicy {
	return SetupPolicy{
		Level: slog.LevelInfo,
		ProcessSink: FileSinkPolicy{
			Enabled: true,
			Path:    filepath.Join(root, "clyde-daemon.jsonl"),
			Rotation: RotationPolicy{
				Enabled:    false,
				MaxSizeMB:  0,
				MaxBackups: 0,
				MaxAgeDays: 0,
				Compress:   nil,
			},
		},
		ConcernRoot:     filepath.Join(root, "logs"),
		ConcernPolicies: nil,
		TranscriptPolicy: TranscriptPolicy{
			Enabled: false,
		},
	}
}

func filterLockFiles(paths []string) []string {
	filtered := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.HasSuffix(path, ".lock") {
			continue
		}
		filtered = append(filtered, path)
	}
	return filtered
}

type logEvent struct {
	Message              string `json:"msg"`
	TraceID              string `json:"trace_id"`
	SpanID               string `json:"span_id"`
	ParentSpanID         string `json:"parent_span_id"`
	RequestID            string `json:"request_id"`
	CursorRequestID      string `json:"cursor_request_id"`
	CursorConversationID string `json:"cursor_conversation_id"`
	CursorGenerationID   string `json:"cursor_generation_id"`
	UpstreamRequestID    string `json:"upstream_request_id"`
	UpstreamResponseID   string `json:"upstream_response_id"`
}

func readSingleEvent(t *testing.T, path string) logEvent {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log %s: %v", path, err)
	}
	line := strings.TrimSpace(string(content))
	var event logEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("unmarshal log event: %v content=%s", err, content)
	}
	return event
}
