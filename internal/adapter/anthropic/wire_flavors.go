package anthropic

// WireHeader is one outbound HTTP header. Name is canonical-cased.
// Value is the literal value observed in the captured reference for
// "constant"-classified headers. For "enum" and "free" headers the
// loader surfaces a sample value; the adapter is responsible for
// computing the real value at runtime.
type WireHeader struct {
	Name           string
	Value          string
	Classification string // "constant" | "enum" | "free"
}

// WireFlavorFeatureVector is the request feature vector used to select a
// learned wire flavor. A configured profile identity takes precedence over
// model matching.
type WireFlavorFeatureVector struct {
	ModelID     string
	WireProfile string
}

// WireFlavor is the captured wire shape for one caller flavor of an
// upstream. Fields are typed to satisfy strict type hygiene; no
// map[string]string or interface{} appears here.
//
// WireFlavor values are no longer compiled in. The
// [WireFlavorsLoader] reads the daemon-owned MITM baseline at runtime
// and projects each captured flavor into a WireFlavor with the same
// mapping the build-time codegen used, so the on-disk baseline is the
// single source of truth for identity drift.
type WireFlavor struct {
	Slug               string
	UserAgent          string
	AnthropicVersion   string
	AnthropicBeta      string
	BetaFlags          []string
	StaticHeaders      []WireHeader
	BodyFields         []string
	BodyFieldsRequired []string
	FeatureVectors     []WireFlavorFeatureVector
	// BillingAttestation is the captured claude-code `cch=<value>`
	// token observed across this flavor's drift records. The egress
	// path substitutes it into the billing/attribution system block in
	// place of the locally-generated placeholder. It is empty when the
	// baseline did not capture a `cch` token, in which case the egress
	// keeps the placeholder value.
	BillingAttestation string
}
