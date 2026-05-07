package slogger

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/correlation"
)

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

	For(ConcernSessionDiscoveryScan).Warn("session.scan.walk_failed", "path", "/tmp/nope")
	For(ConcernDaemonRPCRequests).Info("daemon.rpc.started", "method", "/clyde.v1.Daemon/ListSessions")
	For(ConcernUITUIActions).Debug("tui.input.key", "key", "enter")
	_ = closer.Close()

	assertLogContains(t, filepath.Join(root, "logs", "session", "discovery", "scan.jsonl"), "session.scan.walk_failed")
	assertLogContains(t, filepath.Join(root, "logs", "daemon", "rpc", "requests.jsonl"), "daemon.rpc.started")
	assertLogContains(t, filepath.Join(root, "logs", "ui", "tui", "actions.jsonl"), "tui.input.key")
}

func TestConcernLoggerResolvesDefaultAfterSetup(t *testing.T) {
	root := t.TempDir()
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	early := Concern(ConcernSessionDomainResolve)

	policy := testSetupPolicy(root)
	policy.Level = slog.LevelDebug
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	early.Logger().Info("session.resolve.lazy_logger", "session", "demo")
	_ = closer.Close()

	unifiedLog, err := os.ReadFile(unified)
	if err != nil {
		t.Fatalf("read unified log: %v", err)
	}
	if strings.Contains(string(unifiedLog), `"msg":"INFO session.resolve.lazy_logger`) {
		t.Fatalf("lazy concern logger used bootstrap text logger: %s", unifiedLog)
	}
	if !strings.Contains(string(unifiedLog), `"msg":"session.resolve.lazy_logger"`) {
		t.Fatalf("unified log missing lazy logger event: %s", unifiedLog)
	}
	assertLogContains(t, filepath.Join(root, "logs", "session", "domain", "resolve.jsonl"), "session.resolve.lazy_logger")
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
		TraceID:              "0123456789abcdef0123456789abcdef",
		SpanID:               "0123456789abcdef",
		ParentSpanID:         "fedcba9876543210",
		RequestID:            "req-ctx",
		CursorRequestID:      "cursor-req",
		CursorConversationID: "cursor-conv",
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
	if event.CursorRequestID != corr.CursorRequestID {
		t.Fatalf("cursor_request_id = %q, want %q", event.CursorRequestID, corr.CursorRequestID)
	}
	if event.CursorConversationID != corr.CursorConversationID {
		t.Fatalf("cursor_conversation_id = %q, want %q", event.CursorConversationID, corr.CursorConversationID)
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
		TraceID:            "0123456789abcdef0123456789abcdef",
		SpanID:             "0123456789abcdef",
		ParentSpanID:       "fedcba9876543210",
		RequestID:          "req-ctx",
		CursorGenerationID: "cursor-gen",
		UpstreamRequestID:  "upstream-req",
		UpstreamResponseID: "upstream-resp",
	}
	ctx := correlation.WithContext(context.Background(), corr)
	For(ConcernDaemonRPCRequests).InfoContext(ctx,
		"daemon.rpc.started",
		"trace_id", "explicit-trace",
		"span_id", "explicit-span",
	)
	_ = closer.Close()

	event := readSingleEvent(t, filepath.Join(root, "logs", "daemon", "rpc", "requests.jsonl"))
	if event.Message != "daemon.rpc.started" {
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
	if event.CursorGenerationID != corr.CursorGenerationID {
		t.Fatalf("cursor_generation_id = %q, want %q", event.CursorGenerationID, corr.CursorGenerationID)
	}
	if event.UpstreamRequestID != corr.UpstreamRequestID {
		t.Fatalf("upstream_request_id = %q, want %q", event.UpstreamRequestID, corr.UpstreamRequestID)
	}
	if event.UpstreamResponseID != corr.UpstreamResponseID {
		t.Fatalf("upstream_response_id = %q, want %q", event.UpstreamResponseID, corr.UpstreamResponseID)
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
	For(ConcernDaemonRPCRequests).Debug("daemon.rpc.debug_event")
	_ = closer.Close()

	assertLogContains(t, filepath.Join(root, "logs", "adapter", "models", "catalog.jsonl"), "adapter.models.debug_event")
	assertLogMissing(t, filepath.Join(root, "logs", "daemon", "rpc", "requests.jsonl"), "daemon.rpc.debug_event")
	assertLogMissing(t, filepath.Join(root, "clyde-daemon.jsonl"), "adapter.models.debug_event")
}

func TestSetupWithPolicyAppliesPerConcernRotationOverride(t *testing.T) {
	root := t.TempDir()
	policy := testSetupPolicy(root)
	policy.ProcessSink.Rotation = RotationPolicy{
		Enabled:    true,
		MaxSizeMB:  64,
		MaxBackups: 1,
		MaxAgeDays: 1,
		Compress:   new(false),
	}
	policy.ConcernPolicies = map[string]ConcernPolicy{
		ConcernAdapterModelsCatalog: {
			Enabled: nil,
			Level:   nil,
			Rotation: &RotationPolicy{
				Enabled:    true,
				MaxSizeMB:  1,
				MaxBackups: 2,
				MaxAgeDays: 1,
				Compress:   new(false),
			},
		},
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
	if len(concernLogs) < 2 {
		t.Fatalf("concern rotation did not create backups: %v", concernLogs)
	}
	processLogs, err := filepath.Glob(filepath.Join(root, "clyde-daemon*.jsonl*"))
	if err != nil {
		t.Fatalf("glob process logs: %v", err)
	}
	processLogs = filterLockFiles(processLogs)
	if len(processLogs) != 1 {
		t.Fatalf("process sink should keep global rotation budget, got logs: %v", processLogs)
	}
}

func TestSetupWithPolicyDisablesPerConcernRotation(t *testing.T) {
	root := t.TempDir()
	policy := testSetupPolicy(root)
	policy.ConcernPolicies = map[string]ConcernPolicy{
		ConcernAdapterModelsCatalog: {
			Enabled: nil,
			Level:   nil,
			Rotation: &RotationPolicy{
				Enabled:    false,
				MaxSizeMB:  1,
				MaxBackups: 2,
				MaxAgeDays: 1,
				Compress:   new(false),
			},
		},
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
		t.Fatalf("concern rotation should be disabled, got logs: %v", concernLogs)
	}
}

func TestValidateConcernNamesRejectsUnknownConcern(t *testing.T) {
	err := ValidateConcernNames([]string{
		ConcernAdapterModelsCatalog,
		"adapter.models.",
	})
	if err == nil {
		t.Fatal("ValidateConcernNames accepted unknown concern")
	}
	if !strings.Contains(err.Error(), `unknown concern "adapter.models."`) {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestSetupWithPolicyPreservesDefaultLevelAndTranscriptSummary(t *testing.T) {
	root := t.TempDir()
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	policy := testSetupPolicy(root)
	policy.TranscriptPolicy.Enabled = true
	policy.TranscriptPolicy.Mode = TranscriptModeSummary
	closer, err := SetupWithPolicy(policy)
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	slog.Debug("daemon.debug_default_filtered")
	slog.Info("daemon.info_default_kept")
	slog.Info("adapter.chat.raw", "chat_key", "chat-defaults", "body", "secret-body")
	_ = closer.Close()

	assertLogContains(t, unified, "daemon.info_default_kept")
	assertLogMissing(t, unified, "daemon.debug_default_filtered")
	transcriptPath := filepath.Join(root, "logs", "chats", "chat-defaults.jsonl")
	assertLogContains(t, transcriptPath, "adapter.chat.raw")
	assertLogMissing(t, transcriptPath, "secret-body")
}

func TestConcernForEventCoversPrimaryTree(t *testing.T) {
	cases := map[string]string{
		"adapter.codex.transport.prepared": ConcernAdapterProviderCodexWS,
		"adapter.anthropic.ingress":        ConcernAdapterProviderAnthReq,
		"session.adopt.completed":          ConcernSessionDiscoveryAdopt,
		"session.resolve.tier1_hit":        ConcernSessionDomainResolve,
		"prune.autoname.started":           ConcernDaemonWorkersPrune,
		"mitm.ws.closed":                   ConcernProviderMITMWire,
		"compact.apply.completed":          ConcernCompactApply,
		"mcp.context.loaded":               ConcernMCPServerContext,
	}
	for event, want := range cases {
		if got := concernForEvent(event); got != want {
			t.Fatalf("concernForEvent(%q)=%q want %q", event, got, want)
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
			Mode:    "",
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
