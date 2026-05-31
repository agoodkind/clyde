package mitm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// seedTestCaptureLine is the typed view of one synthetic capture transcript
// record the seed-baseline test feeds to the v2 extractor.
type seedTestCaptureLine struct {
	Kind           string              `json:"kind"`
	Provider       string              `json:"provider"`
	T              int64               `json:"t"`
	URL            string              `json:"url"`
	Path           string              `json:"path"`
	Method         string              `json:"method"`
	RequestHeaders map[string]string   `json:"request_headers"`
	RequestBody    seedTestRequestBody `json:"request_body"`
}

// seedTestRequestBody is the summary-mode body shape the proxy emits.
type seedTestRequestBody struct {
	BodyType string   `json:"body_type"`
	Keys     []string `json:"keys"`
}

func writeSeedTestTranscript(t *testing.T, dir string) string {
	t.Helper()
	headers := map[string]string{
		"User-Agent":        "claude-cli/2.1.123 (external, sdk-cli)",
		"Anthropic-Beta":    "oauth-2025-04-20,claude-code-20250219",
		"Anthropic-Version": "2023-06-01",
	}
	body := seedTestRequestBody{BodyType: "json", Keys: []string{"messages", "model", "system", "tools"}}
	lines := []seedTestCaptureLine{
		{
			Kind: "http_request", Provider: "claude", T: 1700000000,
			URL: "https://api.anthropic.com/v1/messages", Path: "/v1/messages", Method: "POST",
			RequestHeaders: headers, RequestBody: body,
		},
		{
			Kind: "http_request", Provider: "claude", T: 1700000001,
			URL: "https://api.anthropic.com/v1/messages", Path: "/v1/messages", Method: "POST",
			RequestHeaders: headers, RequestBody: body,
		},
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, line := range lines {
		if err := encoder.Encode(line); err != nil {
			t.Fatalf("encode transcript line: %v", err)
		}
	}
	path := filepath.Join(dir, "capture.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func TestSeedBaselineWritesRoundTrippableV2Baseline(t *testing.T) {
	tmp := t.TempDir()
	transcript := writeSeedTestTranscript(t, tmp)
	output := filepath.Join(tmp, "out", "reference-v2.toml")

	result, err := SeedBaseline(context.Background(), transcript, "claude-code", output, []string{"claude-cli"}, nil)
	if err != nil {
		t.Fatalf("SeedBaseline: %v", err)
	}
	if result.Written != output {
		t.Fatalf("written=%q want %q", result.Written, output)
	}
	if result.Upstream != "claude-code" {
		t.Fatalf("upstream=%q want claude-code", result.Upstream)
	}
	if result.Flavors < 1 {
		t.Fatalf("flavor count=%d want at least 1", result.Flavors)
	}

	if _, err := os.Stat(output); err != nil {
		t.Fatalf("baseline not written: %v", err)
	}
	snap, err := LoadSnapshotV2TOML(output)
	if err != nil {
		t.Fatalf("LoadSnapshotV2TOML: %v", err)
	}
	if snap.Upstream.Name != "claude-code" {
		t.Fatalf("upstream name=%q want claude-code", snap.Upstream.Name)
	}
	if len(snap.Flavors) < 1 {
		t.Fatalf("flavor count=%d want at least 1", len(snap.Flavors))
	}
	if snap.Flavors[0].Signature.UserAgent != "claude-cli/2.1.123 (external, sdk-cli)" {
		t.Fatalf("flavor user agent=%q", snap.Flavors[0].Signature.UserAgent)
	}
}

func TestSeedBaselineRequiresUpstreamAndFrom(t *testing.T) {
	if _, err := SeedBaseline(context.Background(), "/some/path.jsonl", "", "", nil, nil); err == nil {
		t.Fatal("expected error when upstream is empty")
	}
	if _, err := SeedBaseline(context.Background(), "", "claude-code", "", nil, nil); err == nil {
		t.Fatal("expected error when from is empty")
	}
}
