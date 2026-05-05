package slogger

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/correlation"
)

func TestSetupWritesConcernLogToHardCodedNestedTree(t *testing.T) {
	root := t.TempDir()
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	t.Setenv(envOverride, unified)

	closer, err := Setup(config.LoggingConfig{
		Level: "debug",
		Rotation: config.LoggingRotation{
			Enabled: new(false),
		},
	}, ProcessRoleDaemon, nil)
	if err != nil {
		t.Fatalf("Setup: %v", err)
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
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	t.Setenv(envOverride, unified)

	closer, err := Setup(config.LoggingConfig{
		Level: "debug",
		Rotation: config.LoggingRotation{
			Enabled: new(false),
		},
	}, ProcessRoleDaemon, nil)
	if err != nil {
		t.Fatalf("Setup: %v", err)
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
	t.Setenv(envOverride, unified)

	early := Concern(ConcernSessionDomainResolve)

	closer, err := Setup(config.LoggingConfig{
		Level: "debug",
		Rotation: config.LoggingRotation{
			Enabled: new(false),
		},
	}, ProcessRoleDaemon, nil)
	if err != nil {
		t.Fatalf("Setup: %v", err)
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
	t.Setenv(envOverride, unified)

	closer, err := Setup(config.LoggingConfig{
		Level: "debug",
		Rotation: config.LoggingRotation{
			Enabled: new(false),
		},
	}, ProcessRoleDaemon, nil)
	if err != nil {
		t.Fatalf("Setup: %v", err)
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
	unified := filepath.Join(root, "clyde-daemon.jsonl")
	t.Setenv(envOverride, unified)

	closer, err := Setup(config.LoggingConfig{
		Level: "debug",
		Rotation: config.LoggingRotation{
			Enabled: new(false),
		},
	}, ProcessRoleDaemon, nil)
	if err != nil {
		t.Fatalf("Setup: %v", err)
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
