package mitm

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

// claudeSystemBillingBody builds a minimal claude messages body whose first
// system block carries the billing header line with the supplied cch token.
func claudeSystemBillingBody(t *testing.T, cch string) []byte {
	t.Helper()
	body := map[string]any{
		"model": "claude-sonnet",
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.123.fp; cc_entrypoint=cli; cch=" + cch + ";",
			},
			map[string]any{"type": "text", "text": "You are a helpful assistant."},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal claude body: %v", err)
	}
	return raw
}

func readDriftRecords(t *testing.T, path string) []CaptureRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open drift file: %v", err)
	}
	defer func() { _ = f.Close() }()
	var records []CaptureRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec CaptureRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("unmarshal drift record: %v", err)
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan drift file: %v", err)
	}
	return records
}

func TestRecordDriftCaptureWritesHTTPRequestWithMaskedSecretsAndVerbatimIdentity(t *testing.T) {
	captureRoot := t.TempDir()
	cfg := config.MITMConfig{
		CaptureDir: captureRoot,
		Drift:      config.MITMDriftConfig{Enabled: true},
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer super-secret-token")
	header.Set("X-Api-Key", "sk-ant-secret")
	header.Set("Cookie", "session=secret")
	header.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	header.Set("Anthropic-Beta", "oauth-2025-04-20,claude-code-20250219")
	header.Set("Anthropic-Version", "2023-06-01")
	header.Set("X-Stainless-Lang", "js")
	header.Set("X-App", "cli")

	proxy := &Proxy{}
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      header,
		body:        claudeSystemBillingBody(t, "abc12345"),
	})

	driftPath := driftCapturePath(captureRoot, string(driftUpstreamClaudeCode))
	records := readDriftRecords(t, driftPath)
	if len(records) != 1 {
		t.Fatalf("record count=%d want 1", len(records))
	}
	rec := records[0]
	if rec.Kind != RecordHTTPRequest {
		t.Fatalf("kind=%q want %q", rec.Kind, RecordHTTPRequest)
	}
	if rec.Method != http.MethodPost || rec.Path != "/v1/messages" {
		t.Fatalf("method/path=%q/%q", rec.Method, rec.Path)
	}
	if rec.URL != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("url=%q", rec.URL)
	}
	for _, secret := range []string{"authorization", "x-api-key", "cookie"} {
		if rec.RequestHeaders[secret] != driftRedactionToken {
			t.Fatalf("header %q=%q want masked %q", secret, rec.RequestHeaders[secret], driftRedactionToken)
		}
	}
	if rec.RequestHeaders["user-agent"] != "claude-cli/2.1.123 (external, sdk-cli)" {
		t.Fatalf("user-agent not verbatim: %q", rec.RequestHeaders["user-agent"])
	}
	if rec.RequestHeaders["anthropic-beta"] != "oauth-2025-04-20,claude-code-20250219" {
		t.Fatalf("anthropic-beta not verbatim: %q", rec.RequestHeaders["anthropic-beta"])
	}
	if rec.RequestHeaders["anthropic-version"] != "2023-06-01" {
		t.Fatalf("anthropic-version not verbatim: %q", rec.RequestHeaders["anthropic-version"])
	}
	if rec.RequestHeaders["x-stainless-lang"] != "js" {
		t.Fatalf("x-stainless-lang not verbatim: %q", rec.RequestHeaders["x-stainless-lang"])
	}
	if rec.RequestHeaders["x-app"] != "cli" {
		t.Fatalf("x-app not verbatim: %q", rec.RequestHeaders["x-app"])
	}
	if rec.BillingAttestation != "abc12345" {
		t.Fatalf("billing attestation=%q want abc12345", rec.BillingAttestation)
	}
	// The body summary must carry the key set and never the raw prompt text.
	var summary captureBodySummary
	if err := json.Unmarshal(rec.RequestBody, &summary); err != nil {
		t.Fatalf("unmarshal body summary: %v", err)
	}
	for _, want := range []string{"messages", "model", "system"} {
		if !containsString(summary.Keys, want) {
			t.Fatalf("body keys missing %q: %v", want, summary.Keys)
		}
	}
	if strings.Contains(string(rec.RequestBody), "helpful assistant") {
		t.Fatalf("body summary leaked prompt text: %s", rec.RequestBody)
	}
}

func TestRecordDriftCaptureCodexCapturesAttestationHeaderVerbatim(t *testing.T) {
	captureRoot := t.TempDir()
	cfg := config.MITMConfig{
		CaptureDir: captureRoot,
		Drift:      config.MITMDriftConfig{Enabled: true},
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer secret")
	header.Set("Originator", "codex_cli_rs")
	header.Set("Openai-Beta", "responses=experimental")
	header.Set("X-Oai-Attestation", "attest-token-xyz")
	header.Set("X-Codex-Turn-Metadata", "{}")

	proxy := &Proxy{}
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "codex",
		method:      http.MethodPost,
		path:        "/v1/responses",
		upstreamURL: "https://api.openai.com/v1/responses",
		header:      header,
		body:        []byte(`{"model":"gpt-5","input":[]}`),
	})

	driftPath := driftCapturePath(captureRoot, string(driftUpstreamCodexCLI))
	records := readDriftRecords(t, driftPath)
	if len(records) != 1 {
		t.Fatalf("record count=%d want 1", len(records))
	}
	rec := records[0]
	if rec.RequestHeaders["authorization"] != driftRedactionToken {
		t.Fatalf("authorization not masked: %q", rec.RequestHeaders["authorization"])
	}
	if rec.RequestHeaders["x-oai-attestation"] != "attest-token-xyz" {
		t.Fatalf("x-oai-attestation not verbatim: %q", rec.RequestHeaders["x-oai-attestation"])
	}
	if rec.RequestHeaders["originator"] != "codex_cli_rs" {
		t.Fatalf("originator not verbatim: %q", rec.RequestHeaders["originator"])
	}
	if rec.BillingAttestation != "" {
		t.Fatalf("codex billing attestation=%q want empty", rec.BillingAttestation)
	}
}

func TestRecordDriftCaptureSkipsWhenDriftDisabled(t *testing.T) {
	captureRoot := t.TempDir()
	cfg := config.MITMConfig{
		CaptureDir: captureRoot,
		Drift:      config.MITMDriftConfig{Enabled: false},
	}
	proxy := &Proxy{}
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      http.Header{},
		body:        []byte(`{"model":"x"}`),
	})
	if _, err := os.Stat(driftCapturePath(captureRoot, string(driftUpstreamClaudeCode))); !os.IsNotExist(err) {
		t.Fatalf("drift file should not exist when disabled, stat err=%v", err)
	}
}

func TestExtractSnapshotV2FromDriftCaptureYieldsFlavorWithAttestation(t *testing.T) {
	captureRoot := t.TempDir()
	cfg := config.MITMConfig{
		CaptureDir: captureRoot,
		Drift:      config.MITMDriftConfig{Enabled: true},
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer secret")
	header.Set("User-Agent", "claude-cli/2.1.123 (external, sdk-cli)")
	header.Set("Anthropic-Beta", "oauth-2025-04-20,claude-code-20250219")

	proxy := &Proxy{}
	proxy.recordDriftCapture(cfg, driftCaptureInput{
		provider:    "claude",
		method:      http.MethodPost,
		path:        "/v1/messages",
		upstreamURL: "https://api.anthropic.com/v1/messages",
		header:      header,
		body:        claudeSystemBillingBody(t, "deadbeef"),
	})

	driftPath := driftCapturePath(captureRoot, string(driftUpstreamClaudeCode))
	snap, err := ExtractSnapshotV2(driftPath, SnapshotV2Options{
		UpstreamName:               "claude-code",
		UpstreamVersion:            "test",
		ProviderFilter:             ProviderForUpstream("claude-code"),
		IncludeUserAgentSubstrings: []string{"claude-cli"}, MaxBodyDepth: 0, EnumThreshold: 0,
	})
	if err != nil {
		t.Fatalf("ExtractSnapshotV2: %v", err)
	}
	if len(snap.Flavors) != 1 {
		t.Fatalf("flavor count=%d want 1", len(snap.Flavors))
	}
	flavor := snap.Flavors[0]
	if flavor.BillingAttestation != "deadbeef" {
		t.Fatalf("flavor billing attestation=%q want deadbeef", flavor.BillingAttestation)
	}
	if len(flavor.Headers) == 0 {
		t.Fatalf("flavor headers empty")
	}
	if len(flavor.Body.Fields) == 0 {
		t.Fatalf("flavor body fields empty")
	}
	hasUA := false
	for _, h := range flavor.Headers {
		if h.Name == "user-agent" {
			hasUA = true
		}
	}
	if !hasUA {
		t.Fatalf("flavor headers missing user-agent: %+v", flavor.Headers)
	}
	bodyKeys := map[string]bool{}
	for _, f := range flavor.Body.Fields {
		bodyKeys[f.Name] = true
	}
	for _, want := range []string{"messages", "model", "system"} {
		if !bodyKeys[want] {
			t.Fatalf("body fields missing %q: %v", want, bodyKeys)
		}
	}
}

func TestSnapshotV2TOMLRoundTripPreservesBillingAttestation(t *testing.T) {
	in := SnapshotV2{
		Upstream: V2Upstream{Name: "claude-code", Version: "v1", CapturedAt: "", RecordCount: 1},
		Flavors: []FlavorShape{
			{
				Slug:        "claude-code-interactive",
				Signature:   V2Signature{UserAgent: "claude-cli/2.1", BetaFingerprint: "", BodyKeys: []string{"model"}},
				RecordCount: 1,
				Methods:     []string{"POST"},
				Paths:       []string{"/v1/messages"},
				Headers: []V2Header{
					{Name: "user-agent", Classification: V2HeaderClassConstant, Presence: V2HeaderPresenceRequired, ObservedValues: []string{"claude-cli/2.1"}, Pattern: "", OccurrenceRate: 1.0},
				},
				Body:               V2Body{BodyType: "json_object", Fields: []V2Field{{Name: "model", Kind: V2FieldKindString, Presence: V2HeaderPresenceRequired, OccurrenceRate: 1.0, SubFields: nil, ItemKind: "", ItemSubFields: nil, SampleValue: ""}}},
				BillingAttestation: "cch-token-9",
			},
		},
	}
	dir := t.TempDir()
	path, err := WriteSnapshotV2TOML(in, dir)
	if err != nil {
		t.Fatalf("WriteSnapshotV2TOML: %v", err)
	}
	out, err := LoadSnapshotV2TOML(path)
	if err != nil {
		t.Fatalf("LoadSnapshotV2TOML: %v", err)
	}
	if len(out.Flavors) != 1 {
		t.Fatalf("flavor count=%d want 1", len(out.Flavors))
	}
	if out.Flavors[0].BillingAttestation != "cch-token-9" {
		t.Fatalf("billing attestation lost in round-trip: %q", out.Flavors[0].BillingAttestation)
	}
}

func TestDriftRotationConfigCarriesCaptureRotation(t *testing.T) {
	compress := true
	rc := driftRotationConfig(config.LoggingRotation{
		Enabled:    nil,
		MaxSizeMB:  16,
		MaxBackups: 3,
		MaxAgeDays: 7,
		Compress:   &compress,
	})
	if rc.MaxSizeMB != 16 || rc.MaxBackups != 3 || rc.MaxAgeDays != 7 {
		t.Fatalf("rotation not carried: %+v", rc)
	}
	if rc.Compress == nil || !*rc.Compress {
		t.Fatalf("compress not carried: %+v", rc.Compress)
	}
}

func TestDriftCapturePathUsesDriftSubdir(t *testing.T) {
	got := driftCapturePath("/tmp/state/mitm", "claude-code")
	want := filepath.Join("/tmp/state/mitm", "drift", "claude-code.jsonl")
	if got != want {
		t.Fatalf("driftCapturePath=%q want %q", got, want)
	}
}
