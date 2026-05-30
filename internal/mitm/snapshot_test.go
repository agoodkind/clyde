package mitm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExtractSnapshotFromSyntheticTranscript(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "capture.jsonl")
	mustWriteLines(t, transcript, []map[string]any{
		{
			"kind": "ws_start",
			"t":    1700000000,
			"url":  "wss://chatgpt.com/backend-api/codex/responses",
			"request_headers": map[string]string{
				"Authorization":           "Bearer eyJabc.def",
				"openai-beta":             "responses_websockets=2026-02-06",
				"originator":              "codex_exec",
				"x-codex-window-id":       "019d-conv:0",
				"x-codex-installation-id": "0c9613dc-cdd4-4733-a798-a59b96181e4f",
				"x-client-request-id":     "019d-conv",
				"session_id":              "019d-conv",
			},
			"response_headers": map[string]string{
				"upgrade": "websocket",
			},
		},
		{
			"kind":        "ws_msg",
			"t":           1700000001,
			"url":         "wss://chatgpt.com/backend-api/codex/responses",
			"from_client": true,
			"text":        `{"type":"response.create","model":"gpt-5.4","generate":false,"input":[],"store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"019d-conv"}`,
		},
		{
			"kind":        "ws_msg",
			"t":           1700000002,
			"url":         "wss://chatgpt.com/backend-api/codex/responses",
			"from_client": true,
			"text":        `{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_warmup","input":[{"type":"message","role":"user"}],"store":false,"stream":true,"tools":[{"type":"function","name":"ReadFile"}]}`,
		},
		{
			"kind":     "ws_end",
			"t":        1700000003,
			"url":      "wss://chatgpt.com/backend-api/codex/responses",
			"messages": 4,
		},
	})

	snap, err := ExtractSnapshot(transcript, SnapshotOptions{
		UpstreamName:    "codex-cli",
		UpstreamVersion: "test-fixture",
	})
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}

	if snap.Upstream.Name != "codex-cli" {
		t.Errorf("upstream name: %q", snap.Upstream.Name)
	}
	if snap.Body.Type != "response.create" {
		t.Errorf("body type: %q", snap.Body.Type)
	}
	if !containsString(snap.Body.FieldNames, "previous_response_id") {
		t.Errorf("expected previous_response_id in body fields, got %v", snap.Body.FieldNames)
	}
	if !containsString(snap.Body.IncludeKeys, "reasoning.encrypted_content") {
		t.Errorf("missing include key: %v", snap.Body.IncludeKeys)
	}
	if !containsString(snap.Body.ToolKinds, "function") {
		t.Errorf("missing tool kind: %v", snap.Body.ToolKinds)
	}
	if snap.FrameSequence.Opening != "warmup_then_real_chained" {
		t.Errorf("opening: %q", snap.FrameSequence.Opening)
	}
	if !snap.FrameSequence.ChainsPrev {
		t.Errorf("expected chains_prev=true")
	}
	if !snap.FrameSequence.Real.HasPrev {
		t.Errorf("expected real.has_prev=true")
	}
	if snap.FrameSequence.Warmup.Generate != "false" {
		t.Errorf("warmup generate: %q", snap.FrameSequence.Warmup.Generate)
	}

	// Identity constants pulled from handshake headers.
	if snap.Constants.Originator != "codex_exec" {
		t.Errorf("originator: %q", snap.Constants.Originator)
	}
	if snap.Constants.OpenAIBeta != "responses_websockets=2026-02-06" {
		t.Errorf("openai-beta: %q", snap.Constants.OpenAIBeta)
	}

	// Volatile values are normalized in the headers list.
	auth := headerValue(snap.Handshake.Headers, "authorization")
	if auth != "<bearer-redacted>" {
		t.Errorf("authorization should be redacted, got %q", auth)
	}
	window := headerValue(snap.Handshake.Headers, "x-codex-window-id")
	if window != "<conversation_id>:0" {
		t.Errorf("window id should be normalized, got %q", window)
	}
}

func TestSnapshotTOMLRoundTrip(t *testing.T) {
	in := Snapshot{
		Upstream: SnapshotUpstream{Name: "codex-cli", Version: "v1"},
		Body:     SnapshotBody{Type: "response.create", FieldNames: []string{"input", "model"}},
		Constants: SnapshotConstants{
			Originator: "codex_exec",
			OpenAIBeta: "responses_websockets=2026-02-06",
		},
	}
	dir := t.TempDir()
	path, err := WriteSnapshotTOML(in, dir)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := LoadSnapshotTOML(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out.Upstream.Name != "codex-cli" {
		t.Errorf("name lost: %q", out.Upstream.Name)
	}
	if out.Constants.Originator != "codex_exec" {
		t.Errorf("originator lost: %q", out.Constants.Originator)
	}
	if !containsString(out.Body.FieldNames, "model") {
		t.Errorf("field_names lost: %v", out.Body.FieldNames)
	}
}

func TestExtractSnapshotFiltersByProvider(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "capture.jsonl")
	mustWriteLines(t, transcript, []map[string]any{
		{
			"provider": "claude",
			"kind":     "http_request",
			"t":        1700000000,
			"url":      "https://api.anthropic.com/v1/messages",
		},
		{
			"provider": "codex",
			"kind":     "ws_start",
			"t":        1700000001,
			"url":      "wss://chatgpt.com/backend-api/codex/responses",
			"request_headers": map[string]string{
				"originator":  "codex_exec",
				"openai-beta": "responses_websockets=2026-02-06",
			},
		},
		{
			"provider":    "codex",
			"kind":        "ws_msg",
			"t":           1700000002,
			"url":         "wss://chatgpt.com/backend-api/codex/responses",
			"from_client": true,
			"text":        `{"type":"response.create","model":"gpt-5.4","input":[]}`,
		},
	})

	snap, err := ExtractSnapshot(transcript, SnapshotOptions{
		UpstreamName:    "codex-cli",
		UpstreamVersion: "test-fixture",
		ProviderFilter:  "codex",
	})
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}
	if snap.Upstream.Name != "codex-cli" {
		t.Fatalf("name=%q want codex-cli", snap.Upstream.Name)
	}
	if snap.Constants.Originator != "codex_exec" {
		t.Fatalf("originator=%q want codex_exec", snap.Constants.Originator)
	}
}

func mustWriteLines(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	for _, rec := range records {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func headerValue(headers []SnapshotHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}
