package mitm

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/mitm"
)

// seedCaptureLine is the typed view of one synthetic capture transcript
// record the seed-baseline test feeds to the v2 extractor. It mirrors
// the request-record fields the extractor reads (kind, provider, path,
// request_headers, request_body in summary-keys mode).
type seedCaptureLine struct {
	Kind           string            `json:"kind"`
	Provider       string            `json:"provider"`
	T              int64             `json:"t"`
	URL            string            `json:"url"`
	Path           string            `json:"path"`
	Method         string            `json:"method"`
	RequestHeaders map[string]string `json:"request_headers"`
	RequestBody    seedRequestBody   `json:"request_body"`
}

// seedRequestBody is the summary-mode body shape the proxy emits: the
// top-level body keys plus a body_type discriminator, without the
// values themselves.
type seedRequestBody struct {
	BodyType string   `json:"body_type"`
	Keys     []string `json:"keys"`
}

func writeSeedTranscript(t *testing.T, dir string) string {
	t.Helper()
	lines := []seedCaptureLine{
		{
			Kind:     "http_request",
			Provider: "claude",
			T:        1700000000,
			URL:      "https://api.anthropic.com/v1/messages",
			Path:     "/v1/messages",
			Method:   "POST",
			RequestHeaders: map[string]string{
				"User-Agent":        "claude-cli/2.1.123 (external, sdk-cli)",
				"Anthropic-Beta":    "oauth-2025-04-20,claude-code-20250219",
				"Anthropic-Version": "2023-06-01",
			},
			RequestBody: seedRequestBody{
				BodyType: "json",
				Keys:     []string{"messages", "model", "system", "tools"},
			},
		},
		{
			Kind:     "http_request",
			Provider: "claude",
			T:        1700000001,
			URL:      "https://api.anthropic.com/v1/messages",
			Path:     "/v1/messages",
			Method:   "POST",
			RequestHeaders: map[string]string{
				"User-Agent":        "claude-cli/2.1.123 (external, sdk-cli)",
				"Anthropic-Beta":    "oauth-2025-04-20,claude-code-20250219",
				"Anthropic-Version": "2023-06-01",
			},
			RequestBody: seedRequestBody{
				BodyType: "json",
				Keys:     []string{"messages", "model", "system", "tools"},
			},
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
	transcript := writeSeedTranscript(t, tmp)
	output := filepath.Join(tmp, "out", "reference-v2.toml")

	out := &bytes.Buffer{}
	factory := &cli.Factory{IOStreams: &cli.IOStreams{Out: out, Err: &bytes.Buffer{}, In: &bytes.Buffer{}}}

	cmd := seedBaselineCommand(factory)
	cmd.SetArgs([]string{
		"--upstream", "claude-code",
		"--from", transcript,
		"--output", output,
		"--include-ua", "claude-cli",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if _, err := os.Stat(output); err != nil {
		t.Fatalf("baseline not written: %v", err)
	}

	snap, err := mitm.LoadSnapshotV2TOML(output)
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

	got := out.String()
	if !strings.Contains(got, "wrote: "+output) {
		t.Fatalf("output missing wrote line: %q", got)
	}
	if !strings.Contains(got, "upstream: claude-code") {
		t.Fatalf("output missing upstream line: %q", got)
	}
	if !strings.Contains(got, "flavors: ") {
		t.Fatalf("output missing flavors line: %q", got)
	}
}

func TestSeedBaselineRequiresUpstreamAndFrom(t *testing.T) {
	out := &bytes.Buffer{}
	factory := &cli.Factory{IOStreams: &cli.IOStreams{Out: out, Err: &bytes.Buffer{}, In: &bytes.Buffer{}}}

	cmd := seedBaselineCommand(factory)
	cmd.SetArgs([]string{"--from", "/does/not/matter.jsonl"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --upstream is missing")
	}
}
