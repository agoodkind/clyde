package mitm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// SnapshotV2Options configures v2 extraction.
type SnapshotV2Options struct {
	UpstreamName    string
	UpstreamVersion string
	ProviderFilter  string
	// MaxBodyDepth caps recursion when walking nested body fields.
	// Default 3.
	MaxBodyDepth int
	// EnumThreshold is the maximum number of distinct observed
	// values before a header is classified V2HeaderClassFree
	// instead of V2HeaderClassEnum. Default 5.
	EnumThreshold int
	// IncludeUserAgentSubstrings, when non-empty, restricts records
	// to those whose User-Agent header contains at least one of the
	// listed substrings (case-insensitive). Useful for extracting a
	// canonical reference from a transcript that mixes the upstream
	// CLI, our adapter, and other clients sharing the proxy.
	IncludeUserAgentSubstrings []string
	// ExcludeUserAgentSubstrings drops records whose User-Agent
	// matches any listed substring (case-insensitive). Applied after
	// IncludeUserAgentSubstrings.
	ExcludeUserAgentSubstrings []string
	// RequireBodyKeys drops records whose top-level request body is
	// missing any of these keys. Useful to demand canonical fields
	// (e.g. metadata, context_management for claude-code).
	RequireBodyKeys []string
	// ForbidBodyKeys drops records whose top-level request body
	// contains any of these keys. Useful to reject non-canonical
	// shapes (e.g. tool_choice for claude-code).
	ForbidBodyKeys []string
}

const (
	defaultMaxBodyDepth  = 3
	defaultEnumThreshold = 5
)

// ExtractSnapshotV2 reads a JSONL transcript and groups records by
// caller flavor. Each flavor becomes a FlavorShape with classified
// headers and nested body sub-shapes.
func ExtractSnapshotV2(path string, opts SnapshotV2Options) (SnapshotV2, error) {
	if opts.MaxBodyDepth <= 0 {
		opts.MaxBodyDepth = defaultMaxBodyDepth
	}
	if opts.EnumThreshold <= 0 {
		opts.EnumThreshold = defaultEnumThreshold
	}
	rawLines, records, err := readCaptureRecordsRaw(path, opts.ProviderFilter)
	if err != nil {
		return SnapshotV2{}, err
	}
	return buildSnapshotV2(rawLines, records, opts)
}

// WriteSnapshotV2TOML persists a v2 snapshot under dir as
// reference-v2.toml. The filename differs from v1's reference.toml
// so both can coexist for the same upstream.
func WriteSnapshotV2TOML(snap SnapshotV2, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("mitm.snapshot_v2.write_mkdir_failed", "concern", "providers.mitm.wire", "dir", dir, "err", err)
		return "", fmt.Errorf("create v2 snapshot dir %s: %w", dir, err)
	}
	out := filepath.Join(dir, "reference-v2.toml")
	raw, err := toml.Marshal(snap)
	if err != nil {
		slog.Warn("mitm.snapshot_v2.write_marshal_failed", "concern", "providers.mitm.wire", "path", out, "err", err)
		return "", fmt.Errorf("marshal v2 snapshot TOML: %w", err)
	}
	if err := os.WriteFile(out, raw, 0o600); err != nil {
		slog.Warn("mitm.snapshot_v2.write_failed", "concern", "providers.mitm.wire", "path", out, "err", err)
		return "", fmt.Errorf("write v2 snapshot TOML %s: %w", out, err)
	}
	return out, nil
}

// LoadSnapshotV2TOML reads a reference-v2.toml back into a typed
// SnapshotV2.
func LoadSnapshotV2TOML(path string) (SnapshotV2, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("mitm.snapshot_v2.load_read_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
		return SnapshotV2{}, fmt.Errorf("read v2 snapshot TOML %s: %w", path, err)
	}
	var snap SnapshotV2
	if err := toml.Unmarshal(raw, &snap); err != nil {
		slog.Warn("mitm.snapshot_v2.load_unmarshal_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
		return SnapshotV2{}, fmt.Errorf("unmarshal v2 snapshot TOML %s: %w", path, err)
	}
	return snap, nil
}

// rawCaptureFields is the typed view of one captured request line as
// a map of named raw JSON fields. Each field stays opaque until a
// specific accessor decodes it; the typed accessors are exposed via
// rawJSONValue helpers below.
type rawCaptureFields map[string]json.RawMessage

// rawRequest pairs a parsed JSON map of one captured request with
// its typed CaptureRecord. The v2 extractor needs both: the typed
// record for stable Kind/T fields, and the raw map for the
// request_body.keys / request_headers nested under summary-mode
// keys the typed schema doesn't expose.
type rawRequest struct {
	fields rawCaptureFields
	record CaptureRecord
}

func buildSnapshotV2(rawLines [][]byte, records []CaptureRecord, opts SnapshotV2Options) (SnapshotV2, error) {
	if len(records) == 0 {
		return SnapshotV2{}, errNoSnapshotRecords
	}

	// Group request records by flavor. Pair each with its raw line so
	// the body walker has the original JSON.
	flavored := map[string][]rawRequest{}
	flavorSigs := map[string]FlavorSignature{}
	earliest := int64(0)

	for i, rec := range records {
		if rec.T > 0 && (earliest == 0 || rec.T < earliest) {
			earliest = rec.T
		}
		if rec.Kind != RecordHTTPRequest {
			continue
		}
		fields := rawCaptureFields{}
		if err := json.Unmarshal(rawLines[i], &fields); err != nil {
			continue
		}
		// Synthesize a richer signature using the raw request_body
		// keys (which the proxy emits in summary mode under
		// request_body.keys but the typed schema does not expose).
		sig := classifyRequestRaw(fields, rec)
		if !uaMatches(sig.UserAgent, opts.IncludeUserAgentSubstrings, opts.ExcludeUserAgentSubstrings) {
			continue
		}
		if !bodyKeyConstraintsMatch(sig.BodyKeys, opts.RequireBodyKeys, opts.ForbidBodyKeys) {
			continue
		}
		slug := sig.FlavorSlug()
		flavored[slug] = append(flavored[slug], rawRequest{fields: fields, record: rec})
		flavorSigs[slug] = sig
	}

	if len(flavored) == 0 {
		return SnapshotV2{}, errNoSnapshotRecords
	}

	snap := SnapshotV2{
		Upstream: V2Upstream{
			Name:        opts.UpstreamName,
			Version:     opts.UpstreamVersion,
			RecordCount: len(records), CapturedAt: "",
		}, Flavors: nil,
	}
	if earliest > 0 {
		snap.Upstream.CapturedAt = time.Unix(earliest, 0).UTC().Format(time.RFC3339)
	}

	// Build one FlavorShape per flavor, sorted by slug for stable
	// output.
	slugs := make([]string, 0, len(flavored))
	for slug := range flavored {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	for _, slug := range slugs {
		flav := buildFlavorShape(slug, flavorSigs[slug], flavored[slug], opts)
		snap.Flavors = append(snap.Flavors, flav)
	}

	return snap, nil
}

// bodyKeyConstraintsMatch enforces the require/forbid lists against
// the observed top-level body keys for a single record.
func bodyKeyConstraintsMatch(observed, require, forbid []string) bool {
	have := make(map[string]bool, len(observed))
	for _, k := range observed {
		have[k] = true
	}
	for _, k := range require {
		if k == "" {
			continue
		}
		if !have[k] {
			return false
		}
	}
	for _, k := range forbid {
		if k == "" {
			continue
		}
		if have[k] {
			return false
		}
	}
	return true
}

// uaMatches applies include/exclude substring filters to a User-Agent.
// Empty include set means include-all. Comparisons are case-insensitive.
func uaMatches(ua string, include, exclude []string) bool {
	low := strings.ToLower(ua)
	for _, ex := range exclude {
		if ex == "" {
			continue
		}
		if strings.Contains(low, strings.ToLower(ex)) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, inc := range include {
		if inc == "" {
			continue
		}
		if strings.Contains(low, strings.ToLower(inc)) {
			return true
		}
	}
	return false
}

// classifyRequestRaw is the v2 classifier. Same signature shape as
// ClassifyRecord but pulls body keys from the raw map (which has the
// summary-mode request_body.keys array the typed CaptureRecord
// schema doesn't expose).
func classifyRequestRaw(fields rawCaptureFields, rec CaptureRecord) FlavorSignature {
	headers := requestHeaderMap(fields)
	ua := lowerStringFromHeaders(headers, "user-agent")
	beta := lowerStringFromHeaders(headers, "anthropic-beta")
	keys := bodyKeysFromFields(fields)
	_ = rec
	return FlavorSignature{
		UserAgent:       ua,
		BetaFingerprint: betaFingerprint(beta),
		BodyKeys:        keys,
	}
}

// requestHeaderMap pulls the request_headers (or legacy headers)
// field from a captured request as a string→string map.
func requestHeaderMap(fields rawCaptureFields) map[string]string {
	if raw, ok := fields["request_headers"]; ok {
		if out := stringMapFromRaw(raw); len(out) > 0 {
			return out
		}
	}
	if raw, ok := fields["headers"]; ok {
		return stringMapFromRaw(raw)
	}
	return nil
}

func lowerStringFromHeaders(headers map[string]string, lower string) string {
	if headers == nil {
		return ""
	}
	for k, v := range headers {
		if strings.ToLower(k) == lower {
			return v
		}
	}
	return ""
}

// bodyKeysFromFields extracts the list of top-level body keys from a
// captured request record's typed field map. Handles both summary
// mode (request_body.keys array) and raw mode (request_body is the
// full object encoded as a JSON object, JSON string, or nested
// fields).
func bodyKeysFromFields(fields rawCaptureFields) []string {
	body, ok := fields["request_body"]
	if !ok {
		return nil
	}
	trimmed := bytes.TrimSpace([]byte(body))
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	switch trimmed[0] {
	case '{':
		var asMap rawCaptureFields
		if err := json.Unmarshal(body, &asMap); err != nil {
			return nil
		}
		if keysRaw, ok := asMap["keys"]; ok {
			var keys []string
			if err := json.Unmarshal(keysRaw, &keys); err == nil && len(keys) > 0 {
				sort.Strings(keys)
				return keys
			}
		}
		out := make([]string, 0, len(asMap))
		for k := range asMap {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	case '"':
		var bodyStr string
		if err := json.Unmarshal(body, &bodyStr); err != nil {
			return nil
		}
		var parsed rawCaptureFields
		if err := json.Unmarshal([]byte(bodyStr), &parsed); err != nil {
			return nil
		}
		out := make([]string, 0, len(parsed))
		for k := range parsed {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

func buildFlavorShape(slug string, sig FlavorSignature, requests []rawRequest, opts SnapshotV2Options) FlavorShape {
	flav := FlavorShape{
		Slug:        slug,
		Signature:   V2Signature(sig),
		RecordCount: len(requests), Methods: nil, Paths: nil, Headers: nil, Body: V2Body{BodyType: "", Fields: nil},
		FeatureVectors:     nil,
		BillingAttestation: "",
	}

	methodSet := map[string]bool{}
	pathSet := map[string]bool{}
	headerObservations := map[string]map[string]int{} // header → value → count
	headerPresenceCount := map[string]int{}           // header → number of records present
	bodyTypeSet := map[string]bool{}
	fieldObservations := map[string]*v2FieldAcc{} // top-level body field → aggregated observation

	for _, req := range requests {
		// The first non-empty attestation token observed in this
		// flavor's records becomes the flavor's attestation. Records
		// share one caller flavor, so any populated token represents the
		// flavor; preferring the first keeps the output stable.
		if flav.BillingAttestation == "" && req.record.BillingAttestation != "" {
			flav.BillingAttestation = req.record.BillingAttestation
		}
		if m := stringFromRaw(req.fields["method"]); m != "" {
			methodSet[m] = true
		}
		if p := stringFromRaw(req.fields["path"]); p != "" {
			pathSet[p] = true
		}
		headers := requestHeaderMap(req.fields)
		seenInThisRecord := map[string]bool{}
		for k, v := range headers {
			lower := strings.ToLower(k)
			seenInThisRecord[lower] = true
			if headerObservations[lower] == nil {
				headerObservations[lower] = map[string]int{}
			}
			headerObservations[lower][v]++
		}
		for h := range seenInThisRecord {
			headerPresenceCount[h]++
		}
		body := req.fields["request_body"]
		if t := bodyTypeFromRaw(body); t != "" {
			bodyTypeSet[t] = true
		}
		walkBodyTopLevel(body, fieldObservations, opts.MaxBodyDepth)
	}

	flav.Methods = sortedStringSet(methodSet)
	flav.Paths = sortedStringSet(pathSet)

	// Headers
	for name, values := range headerObservations {
		flav.Headers = append(flav.Headers, classifyHeader(name, values, headerPresenceCount[name], len(requests), opts))
	}
	sort.Slice(flav.Headers, func(i, j int) bool { return flav.Headers[i].Name < flav.Headers[j].Name })

	// Body
	flav.Body.BodyType = strings.Join(sortedStringSet(bodyTypeSet), ",")
	for name, acc := range fieldObservations {
		flav.Body.Fields = append(flav.Body.Fields, acc.materialize(name, len(requests)))
	}
	sort.Slice(flav.Body.Fields, func(i, j int) bool { return flav.Body.Fields[i].Name < flav.Body.Fields[j].Name })
	flav.FeatureVectors = observedRequestFeatureVectors(requests)

	return flav
}

func observedRequestFeatureVectors(requests []rawRequest) []RequestFeatures {
	observed := make(map[RequestFeatures]bool, len(requests))
	for _, req := range requests {
		features, ok := requestFeaturesFromRawRequest(req)
		if !ok {
			continue
		}
		observed[features] = true
	}
	out := make([]RequestFeatures, 0, len(observed))
	for features := range observed {
		out = append(out, features)
	}
	sort.Slice(out, func(i, j int) bool {
		return requestFeaturesLess(out[i], out[j])
	})
	return out
}

func requestFeaturesFromRawRequest(req rawRequest) (RequestFeatures, bool) {
	if req.record.RequestFeatures != nil {
		features := *req.record.RequestFeatures
		return features, requestFeaturesUsable(features)
	}
	body := req.fields["request_body"]
	if requestBodyIsCaptureSummary(body) {
		return emptyRequestFeatures(), false
	}
	features, err := ExtractRequestFeatures(CapturedRequest{
		RequestHeaders: requestHeaderMap(req.fields),
		RequestBody:    body,
	})
	if err != nil || !requestFeaturesUsable(features) {
		return emptyRequestFeatures(), false
	}
	return features, true
}

func emptyRequestFeatures() RequestFeatures {
	return RequestFeatures{ModelID: ""}
}

func requestFeaturesUsable(features RequestFeatures) bool {
	return strings.TrimSpace(features.ModelID) != ""
}

func requestBodyIsCaptureSummary(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	fields := rawCaptureFields{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	_, hasKeys := fields["keys"]
	_, hasBodyType := fields["body_type"]
	_, hasMode := fields["mode"]
	return hasKeys && (hasBodyType || hasMode)
}

func requestFeaturesLess(left RequestFeatures, right RequestFeatures) bool {
	return left.ModelID < right.ModelID
}

// stringFromRaw decodes a JSON-encoded string. Returns "" when the
// payload is empty, null, or not a string.
func stringFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// bodyTypeFromRaw pulls the body_type discriminator out of a
// request_body payload when the proxy recorded it in summary mode.
// Returns "" otherwise.
func bodyTypeFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	var fields rawCaptureFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	return stringFromRaw(fields["body_type"])
}

// stringMapFromRaw decodes a JSON object with string values into a
// flat map[string]string. Non-string values are skipped.
func stringMapFromRaw(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var fields rawCaptureFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		if s := stringFromRaw(v); s != "" {
			out[k] = s
		}
	}
	return out
}

func sortedStringSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func classifyHeader(name string, values map[string]int, presence, total int, opts SnapshotV2Options) V2Header {
	observed := make([]string, 0, len(values))
	for v := range values {
		observed = append(observed, v)
	}
	sort.Strings(observed)
	classification := V2HeaderClassConstant
	if len(observed) > 1 {
		if len(observed) > opts.EnumThreshold {
			classification = V2HeaderClassFree
		} else {
			classification = V2HeaderClassEnum
		}
	}
	pres := V2HeaderPresenceRequired
	if presence < total {
		pres = V2HeaderPresenceOptional
	}
	rate := 0.0
	if total > 0 {
		rate = float64(presence) / float64(total)
	}
	out := V2Header{
		Name:           name,
		Classification: classification,
		Presence:       pres,
		OccurrenceRate: rate,
		ObservedValues: observed, Pattern: "",
	}
	if classification == V2HeaderClassFree {
		out.Pattern = canonicalHeaderValue(name, observed[0])
		out.ObservedValues = nil
	}
	return out
}

// v2FieldAcc accumulates observations for one top-level body field
// across multiple captured requests.
type v2FieldAcc struct {
	count          int
	kindCounts     map[V2FieldKind]int
	sample         string
	subAcc         map[string]*v2FieldAcc
	itemKindCounts map[V2FieldKind]int
	itemSubAcc     map[string]*v2FieldAcc
}

func newFieldAcc() *v2FieldAcc {
	return &v2FieldAcc{
		kindCounts:     map[V2FieldKind]int{},
		subAcc:         map[string]*v2FieldAcc{},
		itemKindCounts: map[V2FieldKind]int{},
		itemSubAcc:     map[string]*v2FieldAcc{}, count: 0, sample: "",
	}
}

// observe records one observation of this field's value.
func (a *v2FieldAcc) observe(name string, value json.RawMessage, depth int) {
	a.count++
	kind := classifyKind(value)
	a.kindCounts[kind]++
	if a.sample == "" {
		a.sample = sampleValue(name, value)
	}
	if depth <= 0 {
		return
	}
	switch kind {
	case V2FieldKindObject:
		a.observeObjectChildren(value, depth-1)
	case V2FieldKindArray:
		a.observeArrayItems(value, depth-1)
	case V2FieldKindNull, V2FieldKindBool, V2FieldKindNumber, V2FieldKindString, V2FieldKindUnknown:
		// Leaf kinds contribute no nested observations.
	}
}

// observeObjectChildren folds each field of a JSON object value into
// the sub-accumulator map.
func (a *v2FieldAcc) observeObjectChildren(value json.RawMessage, depth int) {
	sub := map[string]json.RawMessage{}
	if err := json.Unmarshal(value, &sub); err != nil {
		return
	}
	for k, child := range sub {
		childAcc, ok := a.subAcc[k]
		if !ok {
			childAcc = newFieldAcc()
			a.subAcc[k] = childAcc
		}
		childAcc.observe(k, child, depth)
	}
}

// observeArrayItems folds each item of a JSON array value into the
// per-item kind counts and per-item sub-accumulator map.
func (a *v2FieldAcc) observeArrayItems(value json.RawMessage, depth int) {
	var items []json.RawMessage
	if err := json.Unmarshal(value, &items); err != nil {
		return
	}
	for _, item := range items {
		itemKind := classifyKind(item)
		a.itemKindCounts[itemKind]++
		if itemKind != V2FieldKindObject {
			continue
		}
		a.observeArrayObjectItem(item, depth)
	}
}

// observeArrayObjectItem folds one object array entry's fields into
// the per-item sub-accumulator map.
func (a *v2FieldAcc) observeArrayObjectItem(item json.RawMessage, depth int) {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(item, &obj); err != nil {
		return
	}
	for k, child := range obj {
		childAcc, ok := a.itemSubAcc[k]
		if !ok {
			childAcc = newFieldAcc()
			a.itemSubAcc[k] = childAcc
		}
		childAcc.observe(k, child, depth)
	}
}

func (a *v2FieldAcc) materialize(name string, totalRecords int) V2Field {
	out := V2Field{
		Name:        name,
		Kind:        dominantKind(a.kindCounts),
		SampleValue: a.sample, Presence: "", OccurrenceRate: 0, SubFields: nil, ItemKind: "", ItemSubFields: nil,
	}
	if a.count >= totalRecords {
		out.Presence = V2HeaderPresenceRequired
	} else {
		out.Presence = V2HeaderPresenceOptional
	}
	if totalRecords > 0 {
		out.OccurrenceRate = float64(a.count) / float64(totalRecords)
	}
	if out.Kind == V2FieldKindObject {
		for sub, acc := range a.subAcc {
			out.SubFields = append(out.SubFields, acc.materialize(sub, a.count))
		}
		sort.Slice(out.SubFields, func(i, j int) bool { return out.SubFields[i].Name < out.SubFields[j].Name })
	}
	if out.Kind == V2FieldKindArray {
		out.ItemKind = dominantKind(a.itemKindCounts)
		for sub, acc := range a.itemSubAcc {
			out.ItemSubFields = append(out.ItemSubFields, acc.materialize(sub, a.count))
		}
		sort.Slice(out.ItemSubFields, func(i, j int) bool { return out.ItemSubFields[i].Name < out.ItemSubFields[j].Name })
	}
	return out
}

// absorbSummaryModeKeys folds summary-mode body keys into the
// accumulator. Returns true when the body matched the summary shape
// (and the caller should stop), false otherwise.
func absorbSummaryModeKeys(fields map[string]json.RawMessage, dst map[string]*v2FieldAcc) bool {
	keysRaw, ok := fields["keys"]
	if !ok {
		return false
	}
	var keys []string
	if err := json.Unmarshal(keysRaw, &keys); err != nil {
		return false
	}
	for _, name := range keys {
		if name == "" {
			continue
		}
		acc, ok := dst[name]
		if !ok {
			acc = newFieldAcc()
			dst[name] = acc
		}
		acc.count++
		acc.kindCounts[V2FieldKindUnknown]++
	}
	return true
}

// walkBodyTopLevel iterates the top-level fields of a captured
// request body and folds each into the field accumulator. Handles
// both raw mode (body is a JSON object) and string-encoded mode
// (body is a JSON-encoded string holding a JSON object).
func walkBodyTopLevel(body json.RawMessage, dst map[string]*v2FieldAcc, maxDepth int) {
	if len(body) == 0 {
		return
	}
	trimmed := bytes.TrimSpace([]byte(body))
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return
	}
	if trimmed[0] == '"' {
		var bodyStr string
		if err := json.Unmarshal(body, &bodyStr); err != nil {
			return
		}
		body = json.RawMessage(bodyStr)
		trimmed = bytes.TrimSpace([]byte(body))
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return
		}
	}
	if trimmed[0] != '{' {
		return
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return
	}
	// Special-case: summary-mode body has request_body.keys plus a
	// few summary fields; treat each summary-key as a top-level body
	// field with kind=unknown (we only know names, not values).
	if absorbSummaryModeKeys(fields, dst) {
		return
	}
	for name, value := range fields {
		acc, ok := dst[name]
		if !ok {
			acc = newFieldAcc()
			dst[name] = acc
		}
		acc.observe(name, value, maxDepth-1)
	}
}

// classifyKind returns the JSON value kind by inspecting the leading
// byte of the raw payload.
func classifyKind(raw json.RawMessage) V2FieldKind {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return V2FieldKindNull
	}
	switch trimmed[0] {
	case '{':
		return V2FieldKindObject
	case '[':
		return V2FieldKindArray
	case '"':
		return V2FieldKindString
	case 't', 'f':
		return V2FieldKindBool
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return V2FieldKindNumber
	}
	return V2FieldKindUnknown
}

// sampleValue returns a single representative rendering for the
// captured value. Secret-bearing field names are redacted. Strings
// over 200 bytes are length-summarized to keep the snapshot
// digestible.
func sampleValue(fieldName string, raw json.RawMessage) string {
	if shouldRedactBodySample(fieldName) {
		return "<redacted>"
	}
	kind := classifyKind(raw)
	switch kind {
	case V2FieldKindString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		if len(s) > 200 {
			return fmt.Sprintf("<string len=%d>", len(s))
		}
		return s
	case V2FieldKindArray:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return ""
		}
		return fmt.Sprintf("<array len=%d>", len(items))
	case V2FieldKindObject:
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(raw, &sub); err != nil {
			return ""
		}
		return fmt.Sprintf("<object keys=%d>", len(sub))
	case V2FieldKindNull:
		return "null"
	case V2FieldKindBool, V2FieldKindNumber, V2FieldKindUnknown:
		return string(bytes.TrimSpace([]byte(raw)))
	}
	return string(bytes.TrimSpace([]byte(raw)))
}

func shouldRedactBodySample(fieldName string) bool {
	name := strings.ToLower(fieldName)
	for _, marker := range []string{"api_key", "authorization", "password", "secret", "token", "account_uuid", "device_id", "session_id", "user_id"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func dominantKind(counts map[V2FieldKind]int) V2FieldKind {
	best := V2FieldKindUnknown
	bestCount := 0
	for k, c := range counts {
		if c > bestCount {
			best = k
			bestCount = c
		}
	}
	return best
}
