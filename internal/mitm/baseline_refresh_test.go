package mitm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTranscriptPathReturnsDriftCaptureFile(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	captureRoot := filepath.Join(stateHome, "clyde", "mitm")
	driftPath := driftCapturePath(captureRoot, "claude-code")
	mustWriteLines(t, driftPath, []map[string]any{{"provider": "claude", "kind": "http_request", "t": 1700000000}})

	got, err := ResolveTranscriptPath(captureRoot, "claude-code")
	if err != nil {
		t.Fatalf("ResolveTranscriptPath: %v", err)
	}
	if got != driftPath {
		t.Fatalf("ResolveTranscriptPath()=%q want %q", got, driftPath)
	}
}

func TestResolveTranscriptPathErrorsWhenDriftFileMissing(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	if _, err := ResolveTranscriptPath(filepath.Join(stateHome, "clyde", "mitm"), "claude-code"); err == nil {
		t.Fatalf("ResolveTranscriptPath: want error for missing drift file, got nil")
	}
}

func TestRefreshBaselineDegradesWhenTranscriptHasNoUsableRecords(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	// A drift file that holds only non-request leg records yields nothing the
	// snapshot extractor can classify, so the tick must skip without error.
	captureRoot := filepath.Join(stateHome, "clyde", "mitm")
	driftPath := driftCapturePath(captureRoot, "claude-code")
	mustWriteLines(t, driftPath, []map[string]any{
		{"concern": "providers.mitm.wire", "msg": "logging.request.leg", "leg": "mitm_complete", "request_id": "req-1"},
	})

	outcome, err := RefreshBaseline(context.TODO(), BaselineRefreshOptions{
		Upstream:     "claude-code",
		CaptureRoot:  captureRoot,
		BaselineRoot: filepath.Join(stateHome, "baselines"),
	})
	if err != nil {
		t.Fatalf("RefreshBaseline: want nil error on degenerate transcript, got %v", err)
	}
	if outcome.Created {
		t.Fatalf("Created=true want false on degenerate transcript")
	}
	if outcome.Updated {
		t.Fatalf("Updated=true want false on degenerate transcript")
	}
}

func TestRefreshBaselineInitializesLocalV2Baseline(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	captureRoot := filepath.Join(stateHome, "clyde", "mitm")
	driftPath := driftCapturePath(captureRoot, "claude-code")
	mustWriteLines(t, driftPath, []map[string]any{
		{
			"provider": "claude",
			"kind":     "http_request",
			"t":        1700000000,
			"url":      "https://api.anthropic.com/v1/messages",
			"path":     "/v1/messages",
			"request_headers": map[string]string{
				"user-agent":        "claude-cli/2.1.123 (external, sdk-cli)",
				"anthropic-beta":    "oauth-2025-04-20,claude-code-20250219",
				"anthropic-version": "2023-06-01",
			},
			"request_body": map[string]any{
				"body_type": "json_object",
				"keys":      []any{"messages", "model", "system", "tools"},
			},
			"billing_attestation": "abc123def",
		},
	})

	outcome, err := RefreshBaseline(context.TODO(), BaselineRefreshOptions{
		Upstream:     "claude-code",
		CaptureRoot:  captureRoot,
		BaselineRoot: filepath.Join(stateHome, "baselines"),
		DriftLogPath: filepath.Join(stateHome, "mitm-drift", "claude-code.jsonl"),
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
	flavor := snap.Flavors[0]
	if flavor.Signature.UserAgent != "claude-cli/2.1.123 (external, sdk-cli)" {
		t.Fatalf("user agent=%q", flavor.Signature.UserAgent)
	}
	if flavor.BillingAttestation != "abc123def" {
		t.Fatalf("billing attestation=%q want abc123def", flavor.BillingAttestation)
	}
}
