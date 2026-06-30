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
	// RequestFeatures carries the prompt-free feature vector derived from the
	// raw request before the drift writer reduces RequestBody to its safe
	// summary.
	RequestFeatures *RequestFeatures `json:"request_features,omitempty"`
}

// DiffMismatch is one field-level disagreement between a reference
// baseline and a freshly captured candidate. The flavor diff
// ([DiffReport]) groups these into mismatch, extra, and missing sets.
type DiffMismatch struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	Reason   string `json:"reason"`
}
