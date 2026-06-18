package mitm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm/capture"
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

// recordDriftCapture upserts one deduped native-request shape into the capture
// store's drift table when [config.MITMDriftConfig.Enabled] is true and the
// provider maps to a known drift upstream. The shape masks true secret headers,
// captures identity/attestation headers verbatim, summarizes the body to its
// field-set (no raw prompt text), and lifts the claude billing attestation. The
// store dedupes by fingerprint and holds rows forever; a repeated identical
// shape only advances its seen_count. The store write is non-blocking, so a
// failed or full-queue drift write never blocks the forward path.
func (p *Proxy) recordDriftCapture(cfg config.MITMConfig, input driftCaptureInput) {
	if !cfg.Drift.Enabled {
		return
	}
	upstream, ok := driftUpstreamForProvider(input.provider)
	if !ok {
		return
	}
	p.store.RecordShape(buildDriftShape(input, upstream))
}

// buildDriftShape assembles the deduped [capture.DriftShape] for one native
// request. The body summary keeps only the body_type discriminator and the
// top-level key set (no raw prompt text). The billing attestation is captured
// only for the claude upstream; codex attestation rides through verbatim in the
// captured x-oai-attestation request header. The fingerprint is the dedup key.
func buildDriftShape(input driftCaptureInput, upstream driftUpstreamName) capture.DriftShape {
	summary := summarizeBody(input.body)
	requestHeaders := driftHeaderMap(input.header)
	bodyRaw, err := json.Marshal(captureBodySummary{
		Mode:     summary.Mode,
		BodyType: summary.BodyType,
		Keys:     summary.Keys,
		Bytes:    0,
		SHA256:   "",
		Messages: 0,
		Input:    0,
		Tools:    0,
		Model:    "",
		ArrayLen: 0,
	})
	if err != nil {
		bodyRaw = json.RawMessage(`null`)
	}
	billing := ""
	if upstream == driftUpstreamClaudeCode {
		billing = driftBillingAttestationFromBody(input.body)
	}
	var featuresRaw json.RawMessage
	model := ""
	if features := driftRequestFeatures(requestHeaders, input.body); features != nil {
		if raw, marshalErr := json.Marshal(features); marshalErr == nil {
			featuresRaw = raw
		}
		model = features.ModelID
	}
	return capture.DriftShape{
		Timestamp:          clock.Now().UTC(),
		Provider:           input.provider,
		Upstream:           string(upstream),
		Fingerprint:        driftFingerprint(input, string(upstream), requestHeaders, summary.Keys, billing != "", model),
		Method:             input.method,
		Path:               input.path,
		URL:                input.upstreamURL,
		RequestHeaders:     requestHeaders,
		RequestBody:        bodyRaw,
		BillingAttestation: billing,
		RequestFeatures:    featuresRaw,
		SeenCount:          0,
	}
}

// driftFingerprint is the dedup key for one native request shape: provider,
// upstream, method, path, the masked header set, the top-level body key set,
// whether a billing attestation is present, and the model. Two requests with the
// same fingerprint are the same wire shape for baseline learning and collapse to
// one stored row.
func driftFingerprint(input driftCaptureInput, upstream string, headers map[string]string, bodyKeys []string, billingPresent bool, model string) string {
	var b strings.Builder
	b.WriteString(input.provider)
	b.WriteByte('\n')
	b.WriteString(upstream)
	b.WriteByte('\n')
	b.WriteString(input.method)
	b.WriteByte('\n')
	b.WriteString(input.path)
	b.WriteByte('\n')
	headerKeys := make([]string, 0, len(headers))
	for k := range headers {
		headerKeys = append(headerKeys, k)
	}
	sort.Strings(headerKeys)
	for _, k := range headerKeys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(headers[k])
		b.WriteByte('\n')
	}
	b.WriteByte('|')
	keys := append([]string(nil), bodyKeys...)
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	if billingPresent {
		b.WriteString("billing")
	}
	b.WriteByte('|')
	b.WriteString(model)
	return sha256Hex([]byte(b.String()))
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
