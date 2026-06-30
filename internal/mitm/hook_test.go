package mitm

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestMatchHookRuleMatchesLiteralHost(t *testing.T) {
	rules := []config.MITMHookRule{
		{
			Name:           "literal",
			MatchHost:      "downloads.cursor.com",
			MatchPathRegex: "",
			MatchMethod:    "",
			Mode:           "",
			Command:        "/usr/local/bin/hook",
			Args:           nil,
			Timeout:        0,
		},
	}
	got, ok := matchHookRule(rules, "downloads.cursor.com", "GET", "/anything")
	if !ok {
		t.Fatalf("expected literal host match, got no match")
	}
	if got.Name != "literal" {
		t.Fatalf("expected literal rule, got %q", got.Name)
	}
}

func TestMatchHookRuleMatchesSuffixHost(t *testing.T) {
	rules := []config.MITMHookRule{
		{
			Name:           "suffix",
			MatchHost:      "*.cursor.com",
			MatchPathRegex: "",
			MatchMethod:    "",
			Mode:           "",
			Command:        "/usr/local/bin/hook",
			Args:           nil,
			Timeout:        0,
		},
	}
	got, ok := matchHookRule(rules, "downloads.cursor.com", "GET", "/x")
	if !ok || got.Name != "suffix" {
		t.Fatalf("expected suffix match on downloads.cursor.com, got name=%q ok=%v", got.Name, ok)
	}
	if _, ok := matchHookRule(rules, "downloads.cursor.sh", "GET", "/x"); ok {
		t.Fatalf("expected no match for downloads.cursor.sh against *.cursor.com")
	}
}

func TestMatchHookRuleAppliesPathAndMethod(t *testing.T) {
	rules := []config.MITMHookRule{
		{
			Name:           "cursor-zip",
			MatchHost:      "downloads.cursor.com",
			MatchPathRegex: `^/production/[a-f0-9]+/darwin/arm64/Cursor-darwin-arm64\.zip$`,
			MatchMethod:    "GET",
			Mode:           config.MITMHookModeTransformResponse,
			Command:        "/usr/local/bin/hook",
			Args:           nil,
			Timeout:        0,
		},
	}
	good := "/production/abcdef0123456789/darwin/arm64/Cursor-darwin-arm64.zip"
	if _, ok := matchHookRule(rules, "downloads.cursor.com", "GET", good); !ok {
		t.Fatalf("expected match for path %q", good)
	}
	if _, ok := matchHookRule(rules, "downloads.cursor.com", "POST", good); ok {
		t.Fatalf("expected method mismatch to reject")
	}
	bad := "/production/abcdef/darwin/arm64/other.zip"
	if _, ok := matchHookRule(rules, "downloads.cursor.com", "GET", bad); ok {
		t.Fatalf("expected path regex mismatch to reject")
	}
}

func TestMatchHookRuleSkipsEmptyCommand(t *testing.T) {
	rules := []config.MITMHookRule{
		{
			Name:           "no-command",
			MatchHost:      "downloads.cursor.com",
			MatchPathRegex: "",
			MatchMethod:    "",
			Mode:           "",
			Command:        "",
			Args:           nil,
			Timeout:        0,
		},
	}
	if _, ok := matchHookRule(rules, "downloads.cursor.com", "GET", "/x"); ok {
		t.Fatalf("expected empty-command rule to be skipped")
	}
}

func TestResolveHookModeDefaults(t *testing.T) {
	if resolveHookMode("") != config.MITMHookModeTransformResponse {
		t.Fatalf("expected empty mode to default to transform_response")
	}
	if resolveHookMode(config.MITMHookModeSynthesize) != config.MITMHookModeSynthesize {
		t.Fatalf("expected synthesize to round-trip")
	}
}

func TestWriteHookResponseStreamsStatusAndBody(t *testing.T) {
	body := []byte("hook-body")
	result := &hookResult{
		Status:        http.StatusAccepted,
		Headers:       http.Header{"Content-Type": []string{"text/plain"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	written, err := writeHookResponse(writer, result, logger)
	if err != nil {
		t.Fatalf("write hook response: %v", err)
	}
	if written <= int64(len(body)) {
		t.Fatalf("written = %d want header plus body", written)
	}
	if !bytes.Contains(output.Bytes(), []byte("HTTP/1.1 202 Accepted")) {
		t.Fatalf("response missing status line: %q", output.String())
	}
	if !bytes.Contains(output.Bytes(), body) {
		t.Fatalf("response missing body: %q", output.String())
	}
}
