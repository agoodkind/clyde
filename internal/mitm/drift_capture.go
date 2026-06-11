package mitm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog"
)

// driftUpstreamName is the typed drift-upstream slug a native provider maps to.
// The drift loop keys per-upstream baseline files by this name, so the capture
// writer must agree with the [config.MITMDriftConfig.Upstreams] keys and with
// [ProviderForUpstream]'s inverse direction.
type driftUpstreamName string

const (
	// driftUpstreamClaudeCode is the drift slug for the claude provider.
	driftUpstreamClaudeCode driftUpstreamName = "claude-code"
	// driftUpstreamCodexCLI is the drift slug for the codex provider.
	driftUpstreamCodexCLI driftUpstreamName = "codex-cli"
)

// driftProviderFamily is the typed routed-provider label the drift writer maps
// to a drift upstream. Switching on this enum (rather than a bare string) keeps
// the provider->upstream mapping a closed, lint-clean set.
type driftProviderFamily string

const (
	driftProviderClaude driftProviderFamily = "claude"
	driftProviderCodex  driftProviderFamily = "codex"
)

// driftUpstreamForProvider maps a routed provider family to its drift-upstream
// slug. The mapping is a closed typed switch (no loose map) so a new provider
// must opt in explicitly; unknown providers return ("", false) and the caller
// skips drift capture for them.
func driftUpstreamForProvider(provider string) (driftUpstreamName, bool) {
	switch driftProviderFamily(strings.ToLower(strings.TrimSpace(provider))) {
	case driftProviderClaude:
		return driftUpstreamClaudeCode, true
	case driftProviderCodex:
		return driftUpstreamCodexCLI, true
	}
	return "", false
}

// driftCapturePath returns the per-upstream drift capture file path under
// captureRoot. The drift refresh loop's [ResolveTranscriptPath] reads the same
// path, so the two must stay in lockstep.
func driftCapturePath(captureRoot, upstream string) string {
	root := strings.TrimSpace(captureRoot)
	if root == "" {
		root = DefaultCaptureRoot()
	}
	return filepath.Join(expandHome(root), "drift", safePathPart(upstream)+".jsonl")
}

// driftRedactionToken is the fixed value the drift writer substitutes for true
// secret header values so a captured record never carries credentials.
const driftRedactionToken = "<redacted>"

// driftSecretHeader enumerates the request header names whose values are true
// secrets and must be masked in drift records. Identity and attestation headers
// are deliberately NOT in this set so they ride through verbatim.
type driftSecretHeader string

const (
	driftSecretAuthorization driftSecretHeader = "authorization"
	driftSecretXAPIKey       driftSecretHeader = "x-api-key"
	driftSecretCookie        driftSecretHeader = "cookie"
)

// isDriftSecretHeader reports whether a lowercased header name names a true
// secret the drift writer must mask.
func isDriftSecretHeader(lowerName string) bool {
	switch driftSecretHeader(lowerName) {
	case driftSecretAuthorization, driftSecretXAPIKey, driftSecretCookie:
		return true
	}
	return false
}

// driftHeaderMap builds the request_headers map for a drift record: every
// inbound header name with its value, with true secrets masked to a fixed token
// and identity/attestation headers (originator, openai-beta, x-oai-attestation,
// x-codex-*, x-app, x-stainless-*, anthropic-beta, anthropic-version,
// User-Agent, and the rest) captured verbatim. Multi-valued headers join with
// ", " to match the wire join used elsewhere.
func driftHeaderMap(header http.Header) map[string]string {
	out := make(map[string]string, len(header))
	for name, values := range header {
		lower := strings.ToLower(name)
		if isDriftSecretHeader(lower) {
			out[lower] = driftRedactionToken
			continue
		}
		out[lower] = strings.Join(values, ", ")
	}
	return out
}

// driftBillingAttestationFromBody extracts the claude-code `cch=<value>` token
// from the request body's first system text block. The first system block is a
// text block whose text begins with `x-anthropic-billing-header:` and carries
// the canonical `cc_version=...; cc_entrypoint=...; cch=<hash>; ...` line. The
// function returns "" when the body is not a claude messages body, the system
// field is absent or not text-bearing, or the billing line carries no cch
// token. It never returns prompt text; only the cch token is surfaced.
func driftBillingAttestationFromBody(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return ""
	}
	systemRaw, ok := fields["system"]
	if !ok {
		return ""
	}
	line := firstSystemBillingLine(systemRaw)
	if line == "" {
		return ""
	}
	return extractDriftBillingCCH(line)
}

// firstSystemBillingLine returns the text of the first system block when that
// text begins with the billing-header marker. Claude's system field is an array
// of typed content blocks; the billing header lives in the first text block.
func firstSystemBillingLine(systemRaw json.RawMessage) string {
	trimmed := bytes.TrimSpace(systemRaw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ""
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(trimmed, &blocks); err != nil || len(blocks) == 0 {
		return ""
	}
	var first struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(blocks[0], &first); err != nil {
		return ""
	}
	if !strings.HasPrefix(first.Text, "x-anthropic-billing-header:") {
		return ""
	}
	return first.Text
}

// extractDriftBillingCCH lifts the value after `cch=` up to the next `;` from a
// billing-header line. Mirrors the adapter's billing extraction so the captured
// token matches what egress replay later reproduces.
func extractDriftBillingCCH(line string) string {
	const marker = "cch="
	_, after, ok := strings.Cut(line, marker)
	if !ok {
		return ""
	}
	value, _, _ := strings.Cut(after, ";")
	return strings.TrimSpace(value)
}

// driftCaptureInput is the typed per-request state the drift writer needs to
// build one CaptureRecord. It is decoupled from [httpCaptureRecordInput] so the
// drift path can run from both the plain-HTTP forward path and the provider
// TLS-intercept path without sharing the leg-emit input shape.
type driftCaptureInput struct {
	provider    string
	method      string
	path        string
	upstreamURL string
	header      http.Header
	body        []byte
}

// recordDriftCapture appends one `http_request` CaptureRecord line to the
// per-upstream drift file when [config.MITMDriftConfig.Enabled] is true and the
// provider maps to a known drift upstream. The record masks true secret headers,
// captures identity/attestation headers verbatim, summarizes the body to its
// field-set (no raw prompt text), and lifts the claude billing attestation when
// present. The file is local-only state and rotates via the MITM capture
// rotation policy. Failures are logged and swallowed so capture never blocks the
// forward path.
func (p *Proxy) recordDriftCapture(cfg config.MITMConfig, input driftCaptureInput) {
	if !cfg.Drift.Enabled {
		return
	}
	upstream, ok := driftUpstreamForProvider(input.provider)
	if !ok {
		return
	}
	captureRoot := strings.TrimSpace(cfg.Drift.CaptureRoot)
	if captureRoot == "" {
		captureRoot = strings.TrimSpace(cfg.CaptureDir)
	}
	if captureRoot == "" {
		captureRoot = DefaultCaptureRoot()
	}
	rec := buildDriftCaptureRecord(input, upstream)
	// writeDriftCaptureRecord logs its own failure at Warn on the MITM wire
	// concern, so the error is swallowed here: a failed drift write must never
	// block the forward path.
	_ = writeDriftCaptureRecord(captureRoot, string(upstream), cfg.Capture.Rotation, rec)
}

// buildDriftCaptureRecord assembles the typed CaptureRecord for one native
// request. The body summary keeps only the body_type discriminator and the
// top-level key set (no raw prompt text). The billing attestation is captured
// only for the claude upstream; codex attestation rides through verbatim in the
// captured x-oai-attestation request header.
func buildDriftCaptureRecord(input driftCaptureInput, upstream driftUpstreamName) CaptureRecord {
	summary := summarizeBody(input.body)
	requestHeaders := driftHeaderMap(input.header)
	requestFeatures := driftRequestFeatures(requestHeaders, input.body)
	bodyRaw, err := json.Marshal(captureBodySummary{
		Mode:     summary.Mode,
		BodyType: summary.BodyType,
		Bytes:    0,
		SHA256:   "",
		Keys:     summary.Keys,
		Messages: 0,
		Input:    0,
		Tools:    0,
		Model:    "",
		ArrayLen: 0,
		Preview:  "",
	})
	if err != nil {
		bodyRaw = json.RawMessage(`null`)
	}
	billing := ""
	if upstream == driftUpstreamClaudeCode {
		billing = driftBillingAttestationFromBody(input.body)
	}
	return CaptureRecord{
		Kind:               RecordHTTPRequest,
		T:                  clock.Now().UTC().Unix(),
		URL:                input.upstreamURL,
		Provider:           input.provider,
		Concern:            "",
		TraceID:            "",
		SpanID:             "",
		ParentSpanID:       "",
		RequestID:          "",
		UpstreamRequestID:  "",
		UpstreamResponseID: "",
		Method:             input.method,
		Path:               input.path,
		Status:             0,
		Headers:            nil,
		BodyLen:            0,
		Body:               nil,
		RequestBody:        bodyRaw,
		RequestHeaders:     requestHeaders,
		ResponseHeaders:    nil,
		FromClient:         false,
		Length:             0,
		Text:               "",
		Seq:                0,
		Messages:           0,
		Err:                "",
		BillingAttestation: billing,
		RequestFeatures:    requestFeatures,
	}
}

func driftRequestFeatures(headers map[string]string, body []byte) *RequestFeatures {
	features, err := ExtractRequestFeatures(CapturedRequest{
		RequestHeaders: headers,
		RequestBody:    json.RawMessage(body),
	})
	if err != nil || strings.TrimSpace(features.ModelID) == "" {
		return nil
	}
	return &features
}

// writeDriftCaptureRecord serializes rec as one JSONL line and appends it to
// the per-upstream drift file, creating parent directories as needed. The write
// goes through a flock-guarded rotating writer so concurrent daemon generations
// during reload never interleave partial records. Each failure is logged at
// Warn on the MITM wire concern before the wrapped error returns, matching the
// rest of the MITM capture surface; the caller swallows the returned error so a
// failed drift write never blocks the forward path.
func writeDriftCaptureRecord(captureRoot, upstream string, rotation config.LoggingRotation, rec CaptureRecord) error {
	path := driftCapturePath(captureRoot, upstream)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("mitm.drift.capture_mkdir_failed", "concern", "providers.mitm.wire", "component", "mitm", "path", filepath.Dir(path), "err", err)
		return fmt.Errorf("create drift capture dir: %w", err)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("mitm.drift.capture_marshal_failed", "concern", "providers.mitm.wire", "component", "mitm", "path", path, "err", err)
		return fmt.Errorf("marshal drift capture record: %w", err)
	}
	line = append(line, '\n')
	writer := gklog.NewLockedWriteCloser(path, gklog.NewLumberjackWriterWithConfig(path, driftRotationConfig(rotation)))
	if writer == nil {
		slog.Warn("mitm.drift.capture_writer_nil", "concern", "providers.mitm.wire", "component", "mitm", "path", path)
		return fmt.Errorf("drift capture writer is nil for %s", path)
	}
	defer func() { _ = writer.Close() }()
	if _, err := writer.Write(line); err != nil {
		slog.Warn("mitm.drift.capture_write_failed", "concern", "providers.mitm.wire", "component", "mitm", "path", path, "err", err)
		return fmt.Errorf("append drift capture record %s: %w", path, err)
	}
	return nil
}

// driftRotationConfig converts the MITM capture rotation policy into the gklog
// rotation config the drift writer uses, falling back to gklog's own defaults
// when the policy leaves the size unset. The drift file is size-bounded the same
// way the rest of the MITM capture surface is.
func driftRotationConfig(rotation config.LoggingRotation) gklog.RotationConfig {
	return gklog.RotationConfig{
		MaxSizeMB:  rotation.MaxSizeMB,
		MaxBackups: rotation.MaxBackups,
		MaxAgeDays: rotation.MaxAgeDays,
		Compress:   rotation.Compress,
		LocalTime:  nil,
	}
}
