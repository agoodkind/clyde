package mitm

import "encoding/json"

// CaptureRecordKind enumerates the JSONL record kinds produced by
// the proxy. New kinds extend this enum, never spill into untyped
// fields.
type CaptureRecordKind string

const (
	// RecordHTTPRequest is part of Clyde's typed adapter surface.
	RecordHTTPRequest CaptureRecordKind = "http_request"
	// RecordHTTPResponse is part of Clyde's typed adapter surface.
	RecordHTTPResponse CaptureRecordKind = "http_response"
	// RecordWSStart is part of Clyde's typed adapter surface.
	RecordWSStart CaptureRecordKind = "ws_start"
	// RecordWSMessage is part of Clyde's typed adapter surface.
	RecordWSMessage CaptureRecordKind = "ws_msg"
	// RecordWSEnd is part of Clyde's typed adapter surface.
	RecordWSEnd CaptureRecordKind = "ws_end"
)

// CaptureRecord is the typed view of one JSONL line. Different
// kinds populate different fields; consumers switch on Kind. The
// raw map representation lives elsewhere; this struct is what every
// snapshot/diff/codegen consumer reads from.
type CaptureRecord struct {
	Kind CaptureRecordKind `json:"kind"`
	T    int64             `json:"t,omitempty"`
	URL  string            `json:"url,omitempty"`
	// Provider names the routed upstream family that produced this
	// record (for example "claude" or "codex"). Always-on capture
	// files can mix multiple providers, so baseline refresh must be
	// able to filter by this field.
	Provider string `json:"provider,omitempty"`
	// Concern names the routed capture bucket for MITM events. Older
	// root transcripts omit it, and snapshot readers ignore it.
	Concern string `json:"concern,omitempty"`

	TraceID            string `json:"trace_id,omitempty"`
	SpanID             string `json:"span_id,omitempty"`
	ParentSpanID       string `json:"parent_span_id,omitempty"`
	RequestID          string `json:"request_id,omitempty"`
	UpstreamRequestID  string `json:"upstream_request_id,omitempty"`
	UpstreamResponseID string `json:"upstream_response_id,omitempty"`

	// HTTP fields.
	Method string `json:"method,omitempty"`
	// Path is the request URL path the snapshot extractor groups by. The
	// drift-capture writer populates it; older transcripts may omit it.
	Path    string            `json:"path,omitempty"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	BodyLen int               `json:"body_len,omitempty"`
	// Body is the filtered inline payload metadata for capture records. Raw
	// sidecar payloads are referenced by explicit raw path fields when raw
	// capture is enabled.
	Body json.RawMessage `json:"body,omitempty"`
	// RequestBody is the filtered request payload summary (a {keys,
	// body_type} object, never raw prompt text) the snapshot extractor
	// reads to classify the body field-set. The drift-capture writer
	// populates it; the v2 extractor keys on the `request_body` JSON
	// field, distinct from the response-side `body` field above.
	RequestBody json.RawMessage `json:"request_body,omitempty"`
	// ws_start fields.
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`

	// ws_msg fields.
	FromClient bool   `json:"from_client,omitempty"`
	Length     int    `json:"len,omitempty"`
	Text       string `json:"text,omitempty"`
	Seq        int    `json:"seq,omitempty"`

	// ws_end fields.
	Messages int    `json:"messages,omitempty"`
	Err      string `json:"err,omitempty"`

	// BillingAttestation carries the claude-code `cch=<value>` token
	// lifted out of the first system text block's
	// `x-anthropic-billing-header:` line. It is captured only on the
	// drift-capture path for the claude provider and is needed later
	// for egress replay; codex attestation rides through verbatim in
	// the `x-oai-attestation` request header instead.
	BillingAttestation string `json:"billing_attestation,omitempty"`
}

// Snapshot is the typed reference for what an upstream client emits
// on the wire. Persisted as TOML under
// `research/<upstream>/snapshots/<version>/reference.toml`. Diff
// runs the adapter's request_builder for an equivalent input and
// compares against this struct field-by-field.
type Snapshot struct {
	Upstream      SnapshotUpstream      `toml:"upstream" json:"upstream"`
	Handshake     SnapshotHandshake     `toml:"handshake" json:"handshake"`
	Body          SnapshotBody          `toml:"body" json:"body"`
	FrameSequence SnapshotFrameSequence `toml:"frame_sequence" json:"frame_sequence"`
	Constants     SnapshotConstants     `toml:"constants" json:"constants"`
}

// SnapshotUpstream is part of Clyde's typed adapter surface.
type SnapshotUpstream struct {
	Name       string `toml:"name" json:"name"`
	Version    string `toml:"version" json:"version"`
	CapturedAt string `toml:"captured_at" json:"captured_at"`
}

// SnapshotHandshake is part of Clyde's typed adapter surface.
type SnapshotHandshake struct {
	URLPattern string           `toml:"url_pattern" json:"url_pattern"`
	Headers    []SnapshotHeader `toml:"headers" json:"headers"`
}

// SnapshotHeader is part of Clyde's typed adapter surface.
type SnapshotHeader struct {
	Name  string `toml:"name" json:"name"`
	Value string `toml:"value" json:"value"`
}

// SnapshotBody is part of Clyde's typed adapter surface.
type SnapshotBody struct {
	Type        string   `toml:"type" json:"type"`
	FieldNames  []string `toml:"field_names" json:"field_names"`
	ToolKinds   []string `toml:"tool_kinds" json:"tool_kinds"`
	IncludeKeys []string `toml:"include_keys" json:"include_keys"`
}

// SnapshotFrameSequence is part of Clyde's typed adapter surface.
type SnapshotFrameSequence struct {
	Opening    string                  `toml:"opening" json:"opening"`
	Warmup     SnapshotFrameDescriptor `toml:"warmup" json:"warmup"`
	Real       SnapshotFrameDescriptor `toml:"real" json:"real"`
	ChainsPrev bool                    `toml:"chains_prev" json:"chains_prev"`
}

// SnapshotFrameDescriptor is part of Clyde's typed adapter surface.
type SnapshotFrameDescriptor struct {
	Generate           string `toml:"generate" json:"generate"`
	HasPrev            bool   `toml:"has_prev" json:"has_prev"`
	InputCountMin      int    `toml:"input_count_min" json:"input_count_min"`
	InputCountMax      int    `toml:"input_count_max" json:"input_count_max"`
	ToolsCountMin      int    `toml:"tools_count_min" json:"tools_count_min"`
	ToolsCountMax      int    `toml:"tools_count_max" json:"tools_count_max"`
	StoreFieldRequired bool   `toml:"store_field_required" json:"store_field_required"`
	StoreValueObserved string `toml:"store_value_observed" json:"store_value_observed"`
}

// SnapshotConstants is part of Clyde's typed adapter surface.
type SnapshotConstants struct {
	Originator              string `toml:"originator" json:"originator"`
	OpenAIBeta              string `toml:"openai_beta" json:"openai_beta"`
	UserAgent               string `toml:"user_agent" json:"user_agent"`
	BetaFeatures            string `toml:"beta_features" json:"beta_features"`
	StainlessPackageVersion string `toml:"stainless_package_version" json:"stainless_package_version"`
}

// DiffReport is the structured output of comparing a captured
// reference against an equivalent payload built by the adapter.
// Empty Mismatches means the two are equivalent under the
// snapshot's contract.
type DiffReport struct {
	Upstream   string         `json:"upstream"`
	Mismatches []DiffMismatch `json:"mismatches"`
	Extra      []DiffMismatch `json:"extra"`
	Missing    []DiffMismatch `json:"missing"`
}

// DiffMismatch is part of Clyde's typed adapter surface.
type DiffMismatch struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	Reason   string `json:"reason"`
}

// HasDiverged reports whether the diff found any mismatch.
func (r DiffReport) HasDiverged() bool {
	return len(r.Mismatches) > 0 || len(r.Extra) > 0 || len(r.Missing) > 0
}
