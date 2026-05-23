package mitm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTranscriptPathPrefersRootCapture(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "capture.jsonl")
	mustWriteLines(t, path, []map[string]any{{"provider": "claude", "kind": "http_request", "t": 1700000000}})

	got, err := ResolveTranscriptPath(root, "claude-code")
	if err != nil {
		t.Fatalf("ResolveTranscriptPath: %v", err)
	}
	if got != path {
		t.Fatalf("ResolveTranscriptPath()=%q want %q", got, path)
	}
}

func TestRefreshBaselineInitializesLocalV2Baseline(t *testing.T) {
	root := t.TempDir()
	captureRoot := root
	if err := os.MkdirAll(captureRoot, 0o755); err != nil {
		t.Fatalf("mkdir capture root: %v", err)
	}
	transcript := filepath.Join(captureRoot, "capture.jsonl")
	mustWriteLines(t, transcript, []map[string]any{
		{
			"provider": "claude",
			"kind":     "http_request",
			"t":        1700000000,
			"url":      "https://api.anthropic.com/v1/messages",
			"path":     "/v1/messages",
			"request_headers": map[string]string{
				"User-Agent":        "claude-cli/2.1.123 (external, sdk-cli)",
				"Anthropic-Beta":    "oauth-2025-04-20,claude-code-20250219",
				"Anthropic-Version": "2023-06-01",
			},
			"request_body": map[string]any{
				"keys": []any{"messages", "model", "system", "tools"},
			},
		},
		{
			"provider": "codex",
			"kind":     "http_request",
			"t":        1700000001,
			"url":      "https://api.openai.com/v1/responses",
			"path":     "/v1/responses",
			"request_headers": map[string]string{
				"User-Agent": "codex/1.0",
			},
			"request_body": map[string]any{
				"keys": []any{"input", "model"},
			},
		},
	})

	outcome, err := RefreshBaseline(context.TODO(), BaselineRefreshOptions{
		Upstream:     "claude-code",
		CaptureRoot:  root,
		BaselineRoot: filepath.Join(root, "baselines"),
		DriftLogPath: filepath.Join(root, "mitm-drift", "claude-code.jsonl"),
		IncludeUA:    []string{"claude-cli"},
	})
	if err != nil {
		t.Fatalf("RefreshBaseline: %v", err)
	}
	if !outcome.Created {
		t.Fatalf("Created=false want true")
	}
	if !outcome.Updated {
		t.Fatalf("Updated=false want true")
	}
	if outcome.SchemaVersion != "v2" {
		t.Fatalf("SchemaVersion=%q want v2", outcome.SchemaVersion)
	}
	if _, err := os.Stat(outcome.BaselinePath); err != nil {
		t.Fatalf("baseline stat: %v", err)
	}
	snap, err := LoadSnapshotV2TOML(outcome.BaselinePath)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	if len(snap.Flavors) != 1 {
		t.Fatalf("flavor count=%d want 1", len(snap.Flavors))
	}
	if snap.Flavors[0].Signature.UserAgent != "claude-cli/2.1.123 (external, sdk-cli)" {
		t.Fatalf("user agent=%q", snap.Flavors[0].Signature.UserAgent)
	}
}
