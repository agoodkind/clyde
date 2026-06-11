package mitm

// SnapshotV2 is the per-flavor wire-shape reference. Each captured
// caller flavor produces one FlavorShape; the Snapshot collects them
// under one upstream so the diff and codegen tools can iterate.
//
// Compared to v1 (the Codex ws shape), v2 adds:
//   - Per-flavor partitioning (claude-cli probe vs interactive vs ...)
//   - Header value classification (constant vs enum vs free-form)
//   - Header presence (required vs optional)
//   - Body field classification with nested sub-shape recording
//   - Body field presence (required across flavor records vs optional)
//
// v1 stays for Codex ws backward compat. v2 is the path forward for
// HTTP-based providers (Anthropic, OpenAI Chat) where one upstream
// has multiple legitimate caller flavors.
type SnapshotV2 struct {
	Upstream V2Upstream    `toml:"upstream" json:"upstream"`
	Flavors  []FlavorShape `toml:"flavors" json:"flavors"`
}

// V2Upstream is part of Clyde's typed adapter surface.
type V2Upstream struct {
	Name        string `toml:"name" json:"name"`
	Version     string `toml:"version" json:"version"`
	CapturedAt  string `toml:"captured_at" json:"captured_at"`
	RecordCount int    `toml:"record_count" json:"record_count"`
}

// FlavorShape is one observed caller-flavor of an upstream.
type FlavorShape struct {
	Slug        string      `toml:"slug" json:"slug"`
	Signature   V2Signature `toml:"signature" json:"signature"`
	RecordCount int         `toml:"record_count" json:"record_count"`
	Methods     []string    `toml:"methods" json:"methods"`
	Paths       []string    `toml:"paths" json:"paths"`
	Headers     []V2Header  `toml:"headers" json:"headers"`
	Body        V2Body      `toml:"body" json:"body"`
	// FeatureVectors records the distinct request feature vectors observed for
	// this flavor across captured drift records.
	FeatureVectors []RequestFeatures `toml:"feature_vectors,omitempty" json:"feature_vectors,omitempty"`
	// BillingAttestation carries the claude-code `cch=<value>`
	// attestation token observed across this flavor's drift records.
	// It is empty for codex flavors, whose attestation rides through
	// as a captured constant `x-oai-attestation` header instead. Later
	// egress-replay slices read this to reproduce the billing line.
	BillingAttestation string `toml:"billing_attestation,omitempty" json:"billing_attestation,omitempty"`
}

// V2Signature is part of Clyde's typed adapter surface.
type V2Signature struct {
	UserAgent       string   `toml:"user_agent" json:"user_agent"`
	BetaFingerprint string   `toml:"beta_fingerprint" json:"beta_fingerprint"`
	BodyKeys        []string `toml:"body_keys" json:"body_keys"`
}

// V2HeaderClassification labels how stable a header value is across
// records in this flavor.
type V2HeaderClassification string

const (
	// V2HeaderClassConstant is part of Clyde's typed adapter surface.
	V2HeaderClassConstant V2HeaderClassification = "constant" // single value across all records
	// V2HeaderClassEnum is part of Clyde's typed adapter surface.
	V2HeaderClassEnum V2HeaderClassification = "enum" // small finite set of observed values
	// V2HeaderClassFree is part of Clyde's typed adapter surface.
	V2HeaderClassFree V2HeaderClassification = "free" // high cardinality, value patterned out
)

// V2HeaderPresence labels whether a header appeared on every record
// of this flavor or only some.
type V2HeaderPresence string

const (
	// V2HeaderPresenceRequired is part of Clyde's typed adapter surface.
	V2HeaderPresenceRequired V2HeaderPresence = "required"
	// V2HeaderPresenceOptional is part of Clyde's typed adapter surface.
	V2HeaderPresenceOptional V2HeaderPresence = "optional"
)

// V2Header is part of Clyde's typed adapter surface.
type V2Header struct {
	Name           string                 `toml:"name" json:"name"`
	Classification V2HeaderClassification `toml:"classification" json:"classification"`
	Presence       V2HeaderPresence       `toml:"presence" json:"presence"`
	ObservedValues []string               `toml:"observed_values" json:"observed_values"`
	Pattern        string                 `toml:"pattern,omitempty" json:"pattern,omitempty"`
	OccurrenceRate float64                `toml:"occurrence_rate" json:"occurrence_rate"`
}

// V2Body is part of Clyde's typed adapter surface.
type V2Body struct {
	BodyType string    `toml:"body_type" json:"body_type"`
	Fields   []V2Field `toml:"fields" json:"fields"`
}

// V2FieldKind labels the JSON kind of a body field's value. Nested
// objects and arrays carry their own SubFields recursively.
type V2FieldKind string

const (
	// V2FieldKindString is part of Clyde's typed adapter surface.
	V2FieldKindString V2FieldKind = "string"
	// V2FieldKindNumber is part of Clyde's typed adapter surface.
	V2FieldKindNumber V2FieldKind = "number"
	// V2FieldKindBool is part of Clyde's typed adapter surface.
	V2FieldKindBool V2FieldKind = "bool"
	// V2FieldKindObject is part of Clyde's typed adapter surface.
	V2FieldKindObject V2FieldKind = "object"
	// V2FieldKindArray is part of Clyde's typed adapter surface.
	V2FieldKindArray V2FieldKind = "array"
	// V2FieldKindNull is part of Clyde's typed adapter surface.
	V2FieldKindNull V2FieldKind = "null"
	// V2FieldKindUnknown is part of Clyde's typed adapter surface.
	V2FieldKindUnknown V2FieldKind = "unknown"
)

// V2Field is part of Clyde's typed adapter surface.
type V2Field struct {
	Name           string           `toml:"name" json:"name"`
	Kind           V2FieldKind      `toml:"kind" json:"kind"`
	Presence       V2HeaderPresence `toml:"presence" json:"presence"`
	OccurrenceRate float64          `toml:"occurrence_rate" json:"occurrence_rate"`
	// SubFields populates when Kind is object. Each entry describes
	// one observed nested key.
	SubFields []V2Field `toml:"sub_fields,omitempty" json:"sub_fields,omitempty"`
	// ItemKind labels the value kind of array elements when Kind is
	// array. Arrays of objects also populate ItemSubFields with the
	// union of nested keys observed across elements.
	ItemKind      V2FieldKind `toml:"item_kind,omitempty" json:"item_kind,omitempty"`
	ItemSubFields []V2Field   `toml:"item_sub_fields,omitempty" json:"item_sub_fields,omitempty"`
	// SampleValue carries one human-readable example value for this
	// field. For strings under 200 chars: the literal value. For
	// longer or structured values: a length / shape summary.
	SampleValue string `toml:"sample_value,omitempty" json:"sample_value,omitempty"`
}
