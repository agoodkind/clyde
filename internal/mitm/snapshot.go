package mitm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
)

// errNoSnapshotRecords is the sentinel the snapshot extractor returns when the
// shape set contains no usable native-request shapes. The drift loop treats
// this as a warn-and-skip condition rather than an infrastructure failure,
// because the corpus may be empty before any native traffic has been captured.
var errNoSnapshotRecords = errors.New("mitm snapshot: no usable drift shapes")

// SnapshotOptions configures snapshot extraction.
type SnapshotOptions struct {
	UpstreamName    string
	UpstreamVersion string
	// MaxBodyDepth caps recursion when walking nested body fields.
	// Default 3.
	MaxBodyDepth int
	// EnumThreshold is the maximum number of distinct observed
	// values before a header is classified HeaderClassFree
	// instead of HeaderClassEnum. Default 5.
	EnumThreshold int
	// IncludeUserAgentSubstrings, when non-empty, restricts records
	// to those whose User-Agent header contains at least one of the
	// listed substrings (case-insensitive). Useful for extracting a
	// canonical reference from a corpus that mixes the upstream CLI,
	// our adapter, and other clients sharing the proxy.
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

// marshalSnapshot serializes a snapshot to the JSON value persisted in the
// capture store's baselines table.
func marshalSnapshot(snap Snapshot) (json.RawMessage, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		slog.Warn("mitm.snapshot.marshal_failed", "concern", "providers.mitm.wire", "upstream", snap.Upstream.Name, "err", err)
		return nil, fmt.Errorf("marshal snapshot json: %w", err)
	}
	return raw, nil
}

// DecodeBaselineSnapshot parses a snapshot from the capture store's baselines
// column JSON. It is the exported boundary the adapter baseline loaders use to
// turn the store's [capture.Store.CurrentBaseline] bytes into a typed
// [Snapshot] without importing the unexported decoder.
func DecodeBaselineSnapshot(raw json.RawMessage) (Snapshot, error) {
	return unmarshalSnapshot(raw)
}

// unmarshalSnapshot parses a snapshot back from the capture store's baselines
// column. An empty payload yields the zero Snapshot.
func unmarshalSnapshot(raw json.RawMessage) (Snapshot, error) {
	var snap Snapshot
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return snap, nil
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		slog.Warn("mitm.snapshot.unmarshal_failed", "concern", "providers.mitm.wire", "err", err)
		return Snapshot{}, fmt.Errorf("unmarshal snapshot json: %w", err)
	}
	return snap, nil
}

// rawCaptureFields is the typed view of one native request as a map of named
// raw JSON fields. Each field stays opaque until a specific accessor decodes
// it.
type rawCaptureFields map[string]json.RawMessage

// rawRequest pairs a synthesized JSON field map of one deduped native request
// with its typed CaptureRecord and dedup weight. The extractor needs the
// typed record for stable BillingAttestation / RequestFeatures, the raw map
// for the request_body.keys / request_headers fields, and the weight so a
// deduped shape with seen_count=N classifies as if N identical raw requests
// had been observed.
type rawRequest struct {
	fields rawCaptureFields
	record CaptureRecord
	weight int
}

// ExtractSnapshotFromShapes groups deduped native-request shapes by caller
// flavor and builds a [Snapshot]. Each shape carries a seen_count weight so a
// single deduped shape produces the same header classification, presence, and
// occurrence rate as seen_count identical raw requests would.
func ExtractSnapshotFromShapes(shapes []capture.DriftShape, opts SnapshotOptions) (Snapshot, error) {
	if opts.MaxBodyDepth <= 0 {
		opts.MaxBodyDepth = defaultMaxBodyDepth
	}
	if opts.EnumThreshold <= 0 {
		opts.EnumThreshold = defaultEnumThreshold
	}
	if len(shapes) == 0 {
		return Snapshot{}, errNoSnapshotRecords
	}

	flavored := map[string][]rawRequest{}
	flavorSigs := map[string]FlavorSignature{}
	totalWeight := 0
	earliest := int64(0)

	for _, shape := range shapes {
		req := rawRequestFromShape(shape)
		ts := shape.Timestamp.Unix()
		if ts > 0 && (earliest == 0 || ts < earliest) {
			earliest = ts
		}
		sig := classifyRequestRaw(req.fields, req.record)
		if !uaMatches(sig.UserAgent, opts.IncludeUserAgentSubstrings, opts.ExcludeUserAgentSubstrings) {
			continue
		}
		if !bodyKeyConstraintsMatch(sig.BodyKeys, opts.RequireBodyKeys, opts.ForbidBodyKeys) {
			continue
		}
		slug := sig.FlavorSlug()
		flavored[slug] = append(flavored[slug], req)
		flavorSigs[slug] = sig
		totalWeight += req.weight
	}

	if len(flavored) == 0 {
		return Snapshot{}, errNoSnapshotRecords
	}

	snap := Snapshot{
		Upstream: Upstream{
			Name:        opts.UpstreamName,
			Version:     opts.UpstreamVersion,
			RecordCount: totalWeight, CapturedAt: "",
		}, Flavors: nil,
	}
	if earliest > 0 {
		snap.Upstream.CapturedAt = time.Unix(earliest, 0).UTC().Format(time.RFC3339)
	}

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

// rawRequestFromShape converts a deduped [capture.DriftShape] into the internal
// rawRequest the flavor aggregator consumes. It synthesizes the field map the
// raw classifiers read (request_headers, request_body, method, path, url) and
// carries the typed record fields (billing attestation, parsed feature vector)
// plus the dedup weight (min 1).
func rawRequestFromShape(shape capture.DriftShape) rawRequest {
	fields := rawCaptureFields{}
	if raw := encodeStringMapRaw(shape.RequestHeaders); raw != nil {
		fields["request_headers"] = raw
	}
	if len(bytes.TrimSpace([]byte(shape.RequestBody))) > 0 {
		fields["request_body"] = shape.RequestBody
	}
	if raw := encodeJSONString(shape.Method); raw != nil {
		fields["method"] = raw
	}
	if raw := encodeJSONString(shape.Path); raw != nil {
		fields["path"] = raw
	}
	if raw := encodeJSONString(shape.URL); raw != nil {
		fields["url"] = raw
	}
	weight := max(shape.SeenCount, 1)
	record := CaptureRecord{
		Kind:               RecordHTTPRequest,
		T:                  shape.Timestamp.Unix(),
		URL:                shape.URL,
		Provider:           shape.Provider,
		Concern:            "",
		TraceID:            "",
		SpanID:             "",
		ParentSpanID:       "",
		RequestID:          "",
		UpstreamRequestID:  "",
		UpstreamResponseID: "",
		Method:             shape.Method,
		Path:               shape.Path,
		Status:             0,
		Headers:            nil,
		BodyLen:            0,
		Body:               nil,
		RequestBody:        shape.RequestBody,
		RequestHeaders:     nil,
		ResponseHeaders:    nil,
		FromClient:         false,
		Length:             0,
		Text:               "",
		Seq:                0,
		Messages:           0,
		Err:                "",
		BillingAttestation: shape.BillingAttestation,
		RequestFeatures:    parseShapeFeatures(shape.RequestFeatures),
	}
	return rawRequest{fields: fields, record: record, weight: weight}
}

// parseShapeFeatures decodes the prompt-free feature vector JSON a shape
// carries. An empty or invalid payload yields nil so the extractor falls back
// to deriving features from the body summary.
func parseShapeFeatures(raw json.RawMessage) *RequestFeatures {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var features RequestFeatures
	if err := json.Unmarshal(raw, &features); err != nil {
		return nil
	}
	return &features
}

func encodeStringMapRaw(m map[string]string) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return raw
}

func encodeJSONString(value string) json.RawMessage {
	if value == "" {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
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

// classifyRequestRaw pulls body keys from the raw map (which has the
// summary-mode request_body.keys array) and the User-Agent / beta headers from
// the request_headers field.
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

func buildFlavorShape(slug string, sig FlavorSignature, requests []rawRequest, opts SnapshotOptions) FlavorShape {
	flav := FlavorShape{
		Slug:        slug,
		Signature:   Signature(sig),
		RecordCount: 0, Methods: nil, Paths: nil, Headers: nil, Body: Body{BodyType: "", Fields: nil},
		FeatureVectors:     nil,
		BillingAttestation: "",
	}

	methodSet := map[string]bool{}
	pathSet := map[string]bool{}
	headerObservations := map[string]map[string]int{} // header → value → weighted count
	headerPresenceCount := map[string]int{}           // header → weighted count present
	bodyTypeSet := map[string]bool{}
	fieldObservations := map[string]*fieldAcc{} // top-level body field → aggregated observation
	totalWeight := 0

	for _, req := range requests {
		totalWeight += req.weight
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
			headerObservations[lower][v] += req.weight
		}
		for h := range seenInThisRecord {
			headerPresenceCount[h] += req.weight
		}
		body := req.fields["request_body"]
		if t := bodyTypeFromRaw(body); t != "" {
			bodyTypeSet[t] = true
		}
		walkBodyTopLevel(body, fieldObservations, opts.MaxBodyDepth, req.weight)
	}

	flav.RecordCount = totalWeight
	flav.Methods = sortedStringSet(methodSet)
	flav.Paths = sortedStringSet(pathSet)

	// Headers
	for name, values := range headerObservations {
		flav.Headers = append(flav.Headers, classifyHeader(name, values, headerPresenceCount[name], totalWeight, opts))
	}
	sort.Slice(flav.Headers, func(i, j int) bool { return flav.Headers[i].Name < flav.Headers[j].Name })

	// Body
	flav.Body.BodyType = strings.Join(sortedStringSet(bodyTypeSet), ",")
	for name, acc := range fieldObservations {
		flav.Body.Fields = append(flav.Body.Fields, acc.materialize(name, totalWeight))
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

func classifyHeader(name string, values map[string]int, presence, total int, opts SnapshotOptions) Header {
	observed := make([]string, 0, len(values))
	for v := range values {
		observed = append(observed, v)
	}
	sort.Strings(observed)
	classification := HeaderClassConstant
	if len(observed) > 1 {
		if len(observed) > opts.EnumThreshold {
			classification = HeaderClassFree
		} else {
			classification = HeaderClassEnum
		}
	}
	pres := HeaderPresenceRequired
	if presence < total {
		pres = HeaderPresenceOptional
	}
	rate := 0.0
	if total > 0 {
		rate = float64(presence) / float64(total)
	}
	// A header is volatile when it is high-cardinality (free) or when its
	// observed values are all churning-shaped (byte sizes, per-session ids,
	// attestation blobs). Corpus dedup may have collapsed many churning values
	// to one representative, so a value-shape check is needed even when the
	// header now looks constant.
	volatile := classification == HeaderClassFree || allValuesVolatile(observed)
	out := Header{
		Name:           name,
		Classification: classification,
		Presence:       pres,
		OccurrenceRate: rate,
		ObservedValues: observed, Pattern: "",
		Volatile: volatile,
	}
	if classification == HeaderClassFree {
		out.Pattern = canonicalHeaderValue(name, observed[0])
		out.ObservedValues = nil
	}
	// Never persist churning values (UUIDs, sizes, attestation blobs) into the
	// baseline; the header is tracked by presence only.
	if volatile {
		out.ObservedValues = nil
	}
	return out
}

// fieldAcc accumulates observations for one top-level body field
// across multiple captured requests. Counts are weighted by each
// request's dedup seen_count.
type fieldAcc struct {
	count          int
	kindCounts     map[FieldKind]int
	sample         string
	subAcc         map[string]*fieldAcc
	itemKindCounts map[FieldKind]int
	itemSubAcc     map[string]*fieldAcc
}

func newFieldAcc() *fieldAcc {
	return &fieldAcc{
		kindCounts:     map[FieldKind]int{},
		subAcc:         map[string]*fieldAcc{},
		itemKindCounts: map[FieldKind]int{},
		itemSubAcc:     map[string]*fieldAcc{}, count: 0, sample: "",
	}
}

// observe records one weighted observation of this field's value. The weight
// is the request's dedup seen_count so a deduped shape counts as if weight
// identical raw requests had carried the field.
func (a *fieldAcc) observe(name string, value json.RawMessage, depth, weight int) {
	a.count += weight
	kind := classifyKind(value)
	a.kindCounts[kind] += weight
	if a.sample == "" {
		a.sample = sampleValue(name, value)
	}
	if depth <= 0 {
		return
	}
	switch kind {
	case FieldKindObject:
		a.observeObjectChildren(value, depth-1, weight)
	case FieldKindArray:
		a.observeArrayItems(value, depth-1, weight)
	case FieldKindNull, FieldKindBool, FieldKindNumber, FieldKindString, FieldKindUnknown:
		// Leaf kinds contribute no nested observations.
	}
}

// observeObjectChildren folds each field of a JSON object value into
// the sub-accumulator map.
func (a *fieldAcc) observeObjectChildren(value json.RawMessage, depth, weight int) {
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
		childAcc.observe(k, child, depth, weight)
	}
}

// observeArrayItems folds each item of a JSON array value into the
// per-item kind counts and per-item sub-accumulator map.
func (a *fieldAcc) observeArrayItems(value json.RawMessage, depth, weight int) {
	var items []json.RawMessage
	if err := json.Unmarshal(value, &items); err != nil {
		return
	}
	for _, item := range items {
		itemKind := classifyKind(item)
		a.itemKindCounts[itemKind] += weight
		if itemKind != FieldKindObject {
			continue
		}
		a.observeArrayObjectItem(item, depth, weight)
	}
}

// observeArrayObjectItem folds one object array entry's fields into
// the per-item sub-accumulator map.
func (a *fieldAcc) observeArrayObjectItem(item json.RawMessage, depth, weight int) {
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
		childAcc.observe(k, child, depth, weight)
	}
}

func (a *fieldAcc) materialize(name string, totalRecords int) Field {
	out := Field{
		Name:        name,
		Kind:        dominantKind(a.kindCounts),
		SampleValue: a.sample, Presence: "", OccurrenceRate: 0, SubFields: nil, ItemKind: "", ItemSubFields: nil,
	}
	if a.count >= totalRecords {
		out.Presence = HeaderPresenceRequired
	} else {
		out.Presence = HeaderPresenceOptional
	}
	if totalRecords > 0 {
		out.OccurrenceRate = float64(a.count) / float64(totalRecords)
	}
	if out.Kind == FieldKindObject {
		for sub, acc := range a.subAcc {
			out.SubFields = append(out.SubFields, acc.materialize(sub, a.count))
		}
		sort.Slice(out.SubFields, func(i, j int) bool { return out.SubFields[i].Name < out.SubFields[j].Name })
	}
	if out.Kind == FieldKindArray {
		out.ItemKind = dominantKind(a.itemKindCounts)
		for sub, acc := range a.itemSubAcc {
			out.ItemSubFields = append(out.ItemSubFields, acc.materialize(sub, a.count))
		}
		sort.Slice(out.ItemSubFields, func(i, j int) bool { return out.ItemSubFields[i].Name < out.ItemSubFields[j].Name })
	}
	return out
}

// absorbSummaryModeKeys folds summary-mode body keys into the
// accumulator with the request weight. Returns true when the body
// matched the summary shape (and the caller should stop), false
// otherwise.
func absorbSummaryModeKeys(fields map[string]json.RawMessage, dst map[string]*fieldAcc, weight int) bool {
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
		acc.count += weight
		acc.kindCounts[FieldKindUnknown] += weight
	}
	return true
}

// walkBodyTopLevel iterates the top-level fields of a captured
// request body and folds each into the field accumulator with the
// request weight. Handles both raw mode (body is a JSON object) and
// string-encoded mode (body is a JSON-encoded string holding a JSON
// object).
func walkBodyTopLevel(body json.RawMessage, dst map[string]*fieldAcc, maxDepth, weight int) {
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
	if absorbSummaryModeKeys(fields, dst, weight) {
		return
	}
	for name, value := range fields {
		acc, ok := dst[name]
		if !ok {
			acc = newFieldAcc()
			dst[name] = acc
		}
		acc.observe(name, value, maxDepth-1, weight)
	}
}

// classifyKind returns the JSON value kind by inspecting the leading
// byte of the raw payload.
func classifyKind(raw json.RawMessage) FieldKind {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return FieldKindNull
	}
	switch trimmed[0] {
	case '{':
		return FieldKindObject
	case '[':
		return FieldKindArray
	case '"':
		return FieldKindString
	case 't', 'f':
		return FieldKindBool
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return FieldKindNumber
	}
	return FieldKindUnknown
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
	case FieldKindString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		if len(s) > 200 {
			return fmt.Sprintf("<string len=%d>", len(s))
		}
		return s
	case FieldKindArray:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return ""
		}
		return fmt.Sprintf("<array len=%d>", len(items))
	case FieldKindObject:
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(raw, &sub); err != nil {
			return ""
		}
		return fmt.Sprintf("<object keys=%d>", len(sub))
	case FieldKindNull:
		return "null"
	case FieldKindBool, FieldKindNumber, FieldKindUnknown:
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

func dominantKind(counts map[FieldKind]int) FieldKind {
	best := FieldKindUnknown
	bestCount := 0
	for k, c := range counts {
		if c > bestCount {
			best = k
			bestCount = c
		}
	}
	return best
}
