package mitmshow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestClassifyIDByShape(t *testing.T) {
	cases := []struct {
		input string
		want  IDKind
	}{
		{"chatcmpl-abcdef0123456789", IDKindClyde},
		{"chatcmpl-", IDKindClyde},
		{"req_01HABCDEFGHJKMNPQRSTVWXYZ", IDKindUpstream},
		{"req_", IDKindUpstream},
		{"550e8400-e29b-41d4-a716-446655440000", IDKindCursor},
		{"550E8400-E29B-41D4-A716-446655440000", IDKindCursor},
		{"unknown-shape", IDKindAny},
		{"", IDKindAny},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := ClassifyID(tc.input)
			if got != tc.want {
				t.Fatalf("ClassifyID(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// fixtureConfig writes a synthetic concern log tree under tmp and returns a
// config that points the lookup at it.
func fixtureConfig(t *testing.T, tmp string) *config.Config {
	t.Helper()
	logsRoot := filepath.Join(tmp, "logs")
	if err := os.MkdirAll(filepath.Join(logsRoot, "adapter", "providers", "anthropic"), 0o755); err != nil {
		t.Fatalf("mkdir adapter/providers/anthropic: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(logsRoot, "adapter", "http"), 0o755); err != nil {
		t.Fatalf("mkdir adapter/http: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(logsRoot, "adapter", "chat"), 0o755); err != nil {
		t.Fatalf("mkdir adapter/chat: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "mitm", "raw", "api.anthropic.com"), 0o755); err != nil {
		t.Fatalf("mkdir mitm raw: %v", err)
	}
	daemonLog := filepath.Join(tmp, "clyde-daemon.jsonl")
	if err := os.WriteFile(daemonLog, []byte(""), 0o600); err != nil {
		t.Fatalf("write daemon log: %v", err)
	}
	return &config.Config{
		Logging: config.LoggingConfig{
			Paths: config.LoggingPaths{
				Daemon: daemonLog,
			},
		},
		MITM: config.MITMConfig{
			CaptureDir: filepath.Join(tmp, "mitm"),
		},
	}
}

func TestRunShowFindsMatchesInRequestAndErrorsLogs(t *testing.T) {
	tmp := t.TempDir()
	cfg := fixtureConfig(t, tmp)
	logsRoot := filepath.Join(tmp, "logs")

	clydeID := "chatcmpl-abc123"
	traceID := "trace-xyz"
	upstreamID := "req_upstream"

	requestLine := fmt.Sprintf(`{"request_id":%q,"trace_id":%q,"upstream_request_id":%q,"event":"hit"}`, clydeID, traceID, upstreamID)
	if err := os.WriteFile(
		filepath.Join(logsRoot, "adapter", "providers", "anthropic", "request.jsonl"),
		[]byte(requestLine+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("write request log: %v", err)
	}

	errorLine := fmt.Sprintf(`{"request_id":%q,"event":"err"}`, clydeID)
	if err := os.WriteFile(
		filepath.Join(logsRoot, "adapter", "http", "errors.jsonl"),
		[]byte(errorLine+"\n"+`{"request_id":"chatcmpl-other"}`+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("write errors log: %v", err)
	}

	output, err := Lookup(context.Background(), cfg, clydeID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	got := RenderText(output)
	if !strings.Contains(got, requestLine) {
		t.Fatalf("request log line missing in output: %q", got)
	}
	if !strings.Contains(got, errorLine) {
		t.Fatalf("errors log line missing in output: %q", got)
	}
	if strings.Contains(got, "chatcmpl-other") {
		t.Fatalf("other request id leaked into output: %q", got)
	}
	if !strings.Contains(got, "id kind   : clyde") {
		t.Fatalf("kind line missing: %q", got)
	}
	if !strings.Contains(got, "trace_id           : "+traceID) {
		t.Fatalf("trace id correlation missing: %q", got)
	}
}

func TestRunShowJSONShape(t *testing.T) {
	tmp := t.TempDir()
	cfg := fixtureConfig(t, tmp)
	logsRoot := filepath.Join(tmp, "logs")

	clydeID := "chatcmpl-json01"
	requestLine := fmt.Sprintf(`{"request_id":%q,"trace_id":"t1"}`, clydeID)
	if err := os.WriteFile(
		filepath.Join(logsRoot, "adapter", "providers", "anthropic", "request.jsonl"),
		[]byte(requestLine+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("write request log: %v", err)
	}

	renderedOutput, err := Lookup(context.Background(), cfg, clydeID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	body, err := json.Marshal(renderedOutput)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ShowOutput
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(body))
	}
	if got.Query != clydeID {
		t.Fatalf("query=%q want %q", got.Query, clydeID)
	}
	if got.Kind != IDKindClyde {
		t.Fatalf("kind=%q want clyde", got.Kind)
	}
	if got.Correlation.ClydeRequestID != clydeID {
		t.Fatalf("correlation.clyde_request_id=%q want %q", got.Correlation.ClydeRequestID, clydeID)
	}
	if got.Correlation.TraceID != "t1" {
		t.Fatalf("correlation.trace_id=%q want t1", got.Correlation.TraceID)
	}
	if len(got.Passes) != 1 {
		t.Fatalf("passes=%d want 1", len(got.Passes))
	}
	requestSection := findSection(t, got.Passes[0].Sections, "adapter anthropic provider request log")
	if len(requestSection.Matches) != 1 || requestSection.Matches[0] != requestLine {
		t.Fatalf("request section matches=%v want [%q]", requestSection.Matches, requestLine)
	}
	errorsSection := findSection(t, got.Passes[0].Sections, "adapter http errors log")
	if len(errorsSection.Matches) != 0 {
		t.Fatalf("errors section should be empty; got %v", errorsSection.Matches)
	}
}

func TestRunShowExpandsToUpstreamRequestID(t *testing.T) {
	tmp := t.TempDir()
	cfg := fixtureConfig(t, tmp)
	logsRoot := filepath.Join(tmp, "logs")

	clydeID := "chatcmpl-fanout01"
	upstreamID := "req_fanout02"
	clydeLine := fmt.Sprintf(`{"request_id":%q,"upstream_request_id":%q}`, clydeID, upstreamID)
	if err := os.WriteFile(
		filepath.Join(logsRoot, "adapter", "providers", "anthropic", "request.jsonl"),
		[]byte(clydeLine+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("write request log: %v", err)
	}
	upstreamErrorLine := fmt.Sprintf(`{"upstream_request_id":%q,"event":"upstream_err"}`, upstreamID)
	if err := os.WriteFile(
		filepath.Join(logsRoot, "adapter", "http", "errors.jsonl"),
		[]byte(upstreamErrorLine+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("write errors log: %v", err)
	}

	renderedOutput, err := Lookup(context.Background(), cfg, clydeID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	body, err := json.Marshal(renderedOutput)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ShowOutput
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(body))
	}
	if len(got.Passes) != 2 {
		t.Fatalf("passes=%d want 2 (initial + expansion)", len(got.Passes))
	}
	if got.Passes[0].ID != clydeID {
		t.Fatalf("first pass id=%q want %q", got.Passes[0].ID, clydeID)
	}
	if got.Passes[1].ID != upstreamID {
		t.Fatalf("second pass id=%q want %q", got.Passes[1].ID, upstreamID)
	}
	expansionRequest := findSection(t, got.Passes[0].Sections, "adapter anthropic provider request log")
	if len(expansionRequest.Matches) != 1 || expansionRequest.Matches[0] != clydeLine {
		t.Fatalf("first pass should match clyde line; got %v", expansionRequest.Matches)
	}
	expansionErrors := findSection(t, got.Passes[1].Sections, "adapter http errors log")
	if len(expansionErrors.Matches) != 1 || expansionErrors.Matches[0] != upstreamErrorLine {
		t.Fatalf("second pass should match upstream error line; got %v", expansionErrors.Matches)
	}
	if got.Correlation.ClydeRequestID != clydeID {
		t.Fatalf("correlation.clyde_request_id=%q want %q", got.Correlation.ClydeRequestID, clydeID)
	}
	if got.Correlation.UpstreamRequestID != upstreamID {
		t.Fatalf("correlation.upstream_request_id=%q want %q", got.Correlation.UpstreamRequestID, upstreamID)
	}
}

func findSection(t *testing.T, sections []Section, name string) Section {
	t.Helper()
	for _, section := range sections {
		if section.Source == name {
			return section
		}
	}
	t.Fatalf("section %q not found", name)
	return Section{Source: "", Path: "", Matches: nil}
}
