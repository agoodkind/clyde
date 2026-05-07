package mitm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestResolveCaptureConcernUsesOrderedRulePrecedence(t *testing.T) {
	t.Parallel()

	rules := []config.MITMCaptureRouteRule{
		{Concern: "cursor.bidi", Provider: "cursor", PathExact: "/aiserver.v1.AiService/BidiAppend"},
		{Concern: "cursor.agent", Provider: "cursor", PathPrefix: "/aiserver.v1.AiService/"},
		{Concern: "unknown"},
	}
	concern := resolveCaptureConcern(rules, captureConcernInput{
		Provider: "cursor",
		Method:   "POST",
		Path:     "/aiserver.v1.AiService/BidiAppend",
	})
	if concern != "cursor.bidi" {
		t.Fatalf("concern = %q want cursor.bidi", concern)
	}
}

func TestResolveCaptureConcernUsesUnknownFallback(t *testing.T) {
	t.Parallel()

	concern := resolveCaptureConcern([]config.MITMCaptureRouteRule{
		{Concern: "cursor.bidi", Provider: "cursor", PathExact: "/aiserver.v1.AiService/BidiAppend"},
		{Concern: "unknown"},
	}, captureConcernInput{
		Provider: "cursor",
		Method:   "GET",
		Path:     "/unmatched",
	})
	if concern != "unknown" {
		t.Fatalf("concern = %q want unknown", concern)
	}
}

func TestResolveCaptureConcernMatchesContentTypeContains(t *testing.T) {
	t.Parallel()

	concern := resolveCaptureConcern([]config.MITMCaptureRouteRule{
		{Concern: "cursor.protobuf", Provider: "cursor", ContentTypeContains: "protobuf"},
		{Concern: "unknown"},
	}, captureConcernInput{
		Provider:           "cursor",
		Path:               "/whatever",
		RequestContentType: "application/protobuf; proto=cursor",
	})
	if concern != "cursor.protobuf" {
		t.Fatalf("concern = %q want cursor.protobuf", concern)
	}
}

func TestSafePathPartRendersConcernPathSafely(t *testing.T) {
	t.Parallel()

	got := safePathPart("../Cursor Agent/../../raw?x=1")
	if strings.Contains(got, "/") || strings.Contains(got, "..") || strings.Contains(got, "?") {
		t.Fatalf("safe path part = %q, want no separators or traversal markers", got)
	}
	if got == "" || got == "root" {
		t.Fatalf("safe path part = %q, want rendered content", got)
	}
}

func TestAppendCursorCaptureEventWritesRootAndConcernIndexes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	event := cursorCaptureEvent{
		Kind:            "cursor_tls_http",
		Provider:        "cursor",
		Concern:         "cursor.account",
		Host:            "api2.cursor.sh",
		Method:          "GET",
		Path:            "/account",
		RequestRawPath:  filepath.Join(dir, "concerns", "cursor.account", "raw", "api2.cursor.sh", "request.raw"),
		ResponseRawPath: filepath.Join(dir, "concerns", "cursor.account", "raw", "api2.cursor.sh", "response.raw"),
	}
	if err := appendCursorCaptureEvent(dir, event, CaptureFilePolicy{}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	rootRecord := readSingleCursorEvent(t, filepath.Join(dir, "capture.jsonl"))
	if rootRecord.Concern != "cursor.account" {
		t.Fatalf("root concern = %q want cursor.account", rootRecord.Concern)
	}
	concernRecord := readSingleCursorEvent(t, filepath.Join(dir, "concerns", "cursor.account", "capture.jsonl"))
	if concernRecord.Path != "/account" {
		t.Fatalf("concern path = %q want /account", concernRecord.Path)
	}
}

func TestNextRawCapturePathsUsesConcernDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	proxy := &Proxy{rawCaptureSeq: atomic.Uint64{}}
	requestPath, responsePath, err := proxy.nextRawCapturePaths(dir, "cursor.filesync", "API2.Cursor.SH", "/v1/files/../sync?x=1")
	if err != nil {
		t.Fatalf("next raw capture paths: %v", err)
	}
	wantDir := filepath.Join(dir, "concerns", "cursor.filesync", "raw", "api2.cursor.sh")
	if filepath.Dir(requestPath) != wantDir {
		t.Fatalf("request dir = %q want %q", filepath.Dir(requestPath), wantDir)
	}
	if filepath.Dir(responsePath) != wantDir {
		t.Fatalf("response dir = %q want %q", filepath.Dir(responsePath), wantDir)
	}
	if _, err := os.Stat(wantDir); err != nil {
		t.Fatalf("stat raw dir: %v", err)
	}
}

func readSingleCursorEvent(t *testing.T, path string) cursorCaptureEvent {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("%s lines = %d want 1", path, len(lines))
	}
	var event cursorCaptureEvent
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	return event
}
