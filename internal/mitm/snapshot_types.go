package mitm

// Snapshot is the per-flavor wire-shape reference. Each captured caller
// flavor produces one FlavorShape; the Snapshot collects them under one
// upstream so the diff tools can iterate.
//
// A snapshot adds, over a flat shape:
//   - Per-flavor partitioning (claude-cli probe vs interactive vs ...)
//   - Header value classification (constant vs enum vs free-form)
//   - Header presence (required vs optional)
//   - Body field classification with nested sub-shape recording
//   - Body field presence (required across flavor records vs optional)
//
// It is the path forward for HTTP-based providers (Anthropic, OpenAI
// Chat) where one upstream has multiple legitimate caller flavors. The
// snapshot is the JSON value persisted in the capture store's baselines
// table, so the JSON struct tags are the on-disk contract.
type Snapshot struct {
	Upstream Upstream      `json:"upstream"`
	Flavors  []FlavorShape `json:"flavors"`
}

// Upstream is part of Clyde's typed adapter surface.
type Upstream struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	CapturedAt  string `json:"captured_at"`
	RecordCount int    `json:"record_count"`
}

// FlavorShape is one observed caller-flavor of an upstream.
type FlavorShape struct {
	Slug        string    `json:"slug"`
	Signature   Signature `json:"signature"`
	RecordCount int       `json:"record_count"`
	Methods     []string  `json:"methods"`
	Paths       []string  `json:"paths"`
	Headers     []Header  `json:"headers"`
	Body        Body      `json:"body"`
	// FeatureVectors records the distinct request feature vectors observed for
	// this flavor across captured drift records.
	FeatureVectors []RequestFeatures `json:"feature_vectors,omitempty"`
	// BillingAttestation carries the claude-code `cch=<value>`
	// attestation token observed across this flavor's drift records.
	// It is empty for codex flavors, whose attestation rides through
	// as a captured constant `x-oai-attestation` header instead. Later
	// egress-replay slices read this to reproduce the billing line.
	BillingAttestation string `json:"billing_attestation,omitempty"`
}

// Signature is part of Clyde's typed adapter surface.
type Signature struct {
	UserAgent       string   `json:"user_agent"`
	BetaFingerprint string   `json:"beta_fingerprint"`
	BodyKeys        []string `json:"body_keys"`
}

// HeaderClassification labels how stable a header value is across
// records in this flavor.
type HeaderClassification string

const (
	// HeaderClassConstant is part of Clyde's typed adapter surface.
	HeaderClassConstant HeaderClassification = "constant" // single value across all records
	// HeaderClassEnum is part of Clyde's typed adapter surface.
	HeaderClassEnum HeaderClassification = "enum" // small finite set of observed values
	// HeaderClassFree is part of Clyde's typed adapter surface.
	HeaderClassFree HeaderClassification = "free" // high cardinality, value patterned out
)

// HeaderPresence labels whether a header appeared on every record
// of this flavor or only some.
type HeaderPresence string

const (
	// HeaderPresenceRequired is part of Clyde's typed adapter surface.
	HeaderPresenceRequired HeaderPresence = "required"
	// HeaderPresenceOptional is part of Clyde's typed adapter surface.
	HeaderPresenceOptional HeaderPresence = "optional"
)

// Header is part of Clyde's typed adapter surface.
type Header struct {
	Name           string               `json:"name"`
	Classification HeaderClassification `json:"classification"`
	Presence       HeaderPresence       `json:"presence"`
	ObservedValues []string             `json:"observed_values"`
	Pattern        string               `json:"pattern,omitempty"`
	OccurrenceRate float64              `json:"occurrence_rate"`
	// Volatile marks a header whose value churns per request (a byte size,
	// per-session id, or attestation blob) and so carries no wire identity.
	// The diff tracks only such a header's presence, never its value or class,
	// so request-size and session noise never reads as baseline drift.
	Volatile bool `json:"volatile,omitempty"`
}

// Body is part of Clyde's typed adapter surface.
type Body struct {
	BodyType string  `json:"body_type"`
	Fields   []Field `json:"fields"`
}

// FieldKind labels the JSON kind of a body field's value. Nested
// objects and arrays carry their own SubFields recursively.
type FieldKind string

const (
	// FieldKindString is part of Clyde's typed adapter surface.
	FieldKindString FieldKind = "string"
	// FieldKindNumber is part of Clyde's typed adapter surface.
	FieldKindNumber FieldKind = "number"
	// FieldKindBool is part of Clyde's typed adapter surface.
	FieldKindBool FieldKind = "bool"
	// FieldKindObject is part of Clyde's typed adapter surface.
	FieldKindObject FieldKind = "object"
	// FieldKindArray is part of Clyde's typed adapter surface.
	FieldKindArray FieldKind = "array"
	// FieldKindNull is part of Clyde's typed adapter surface.
	FieldKindNull FieldKind = "null"
	// FieldKindUnknown is part of Clyde's typed adapter surface.
	FieldKindUnknown FieldKind = "unknown"
)

// Field is part of Clyde's typed adapter surface.
type Field struct {
	Name           string         `json:"name"`
	Kind           FieldKind      `json:"kind"`
	Presence       HeaderPresence `json:"presence"`
	OccurrenceRate float64        `json:"occurrence_rate"`
	// SubFields populates when Kind is object. Each entry describes
	// one observed nested key.
	SubFields []Field `json:"sub_fields,omitempty"`
	// ItemKind labels the value kind of array elements when Kind is
	// array. Arrays of objects also populate ItemSubFields with the
	// union of nested keys observed across elements.
	ItemKind      FieldKind `json:"item_kind,omitempty"`
	ItemSubFields []Field   `json:"item_sub_fields,omitempty"`
	// SampleValue carries one human-readable example value for this
	// field. For strings under 200 chars: the literal value. For
	// longer or structured values: a length / shape summary.
	SampleValue string `json:"sample_value,omitempty"`
}
