package mitm

import (
	"sort"
	"strings"
)

// DiffSnapshots compares two snapshots for parity. The first is the
// local baseline or explicit reference; the second is the
// just-captured (or just-built) snapshot to validate. Use this for
// the "current capture vs baseline" path. The Mismatches list calls
// out fields that
// disagree, Missing flags constants present in the reference but
// absent in the candidate, and Extra flags fields the candidate
// adds.
func DiffSnapshots(reference, candidate Snapshot) DiffReport {
	report := DiffReport{Upstream: reference.Upstream.Name, Mismatches: nil, Extra: nil, Missing: nil}

	addMismatch := func(field, expected, got, reason string) {
		report.Mismatches = append(report.Mismatches, DiffMismatch{
			Field: field, Expected: expected, Got: got, Reason: reason,
		})
	}

	if reference.Body.Type != candidate.Body.Type {
		addMismatch("body.type", reference.Body.Type, candidate.Body.Type, "frame type drift")
	}

	report.Missing, report.Extra = stringSetDiff(
		reference.Body.FieldNames, candidate.Body.FieldNames, "body.field_names",
		report.Missing, report.Extra,
	)
	report.Missing, report.Extra = stringSetDiff(
		reference.Body.IncludeKeys, candidate.Body.IncludeKeys, "body.include_keys",
		report.Missing, report.Extra,
	)
	report.Missing, report.Extra = stringSetDiff(
		reference.Body.ToolKinds, candidate.Body.ToolKinds, "body.tool_kinds",
		report.Missing, report.Extra,
	)

	if reference.FrameSequence.Opening != candidate.FrameSequence.Opening {
		addMismatch("frame_sequence.opening",
			reference.FrameSequence.Opening, candidate.FrameSequence.Opening,
			"opening pattern drift")
	}
	if reference.FrameSequence.ChainsPrev != candidate.FrameSequence.ChainsPrev {
		addMismatch("frame_sequence.chains_prev",
			boolText(reference.FrameSequence.ChainsPrev),
			boolText(candidate.FrameSequence.ChainsPrev),
			"previous_response_id chaining behavior drift")
	}
	if reference.FrameSequence.Real.HasPrev != candidate.FrameSequence.Real.HasPrev {
		addMismatch("frame_sequence.real.has_prev",
			boolText(reference.FrameSequence.Real.HasPrev),
			boolText(candidate.FrameSequence.Real.HasPrev),
			"real frame prev requirement drift")
	}
	if reference.FrameSequence.Warmup.Generate != candidate.FrameSequence.Warmup.Generate {
		addMismatch("frame_sequence.warmup.generate",
			reference.FrameSequence.Warmup.Generate,
			candidate.FrameSequence.Warmup.Generate,
			"warmup generate flag drift")
	}

	report.Missing, report.Extra = headerSetDiff(
		reference.Handshake.Headers, candidate.Handshake.Headers,
		report.Missing, report.Extra,
	)

	for field, refVal := range constantsAsMap(reference.Constants) {
		candVal := constantsAsMap(candidate.Constants)[field]
		if refVal != "" && refVal != candVal {
			addMismatch("constants."+field, refVal, candVal, "wire constant drift")
		}
	}

	return report
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func constantsAsMap(c SnapshotConstants) map[string]string {
	return map[string]string{
		"originator":                c.Originator,
		"openai_beta":               c.OpenAIBeta,
		"user_agent":                c.UserAgent,
		"beta_features":             c.BetaFeatures,
		"stainless_package_version": c.StainlessPackageVersion,
	}
}

func stringSetDiff(reference, candidate []string, field string, missing, extra []DiffMismatch) ([]DiffMismatch, []DiffMismatch) {
	refSet := map[string]bool{}
	for _, v := range reference {
		refSet[v] = true
	}
	candSet := map[string]bool{}
	for _, v := range candidate {
		candSet[v] = true
	}
	for v := range refSet {
		if !candSet[v] {
			missing = append(missing, DiffMismatch{Field: field, Expected: v, Reason: "candidate missing key", Got: ""})
		}
	}
	for v := range candSet {
		if !refSet[v] {
			extra = append(extra, DiffMismatch{Field: field, Got: v, Reason: "candidate has extra key", Expected: ""})
		}
	}
	return missing, extra
}

func headerSetDiff(reference, candidate []SnapshotHeader, missing, extra []DiffMismatch) ([]DiffMismatch, []DiffMismatch) {
	refMap := map[string]string{}
	for _, h := range reference {
		refMap[h.Name] = h.Value
	}
	candMap := map[string]string{}
	for _, h := range candidate {
		candMap[h.Name] = h.Value
	}
	for name, refVal := range refMap {
		candVal, ok := candMap[name]
		if !ok {
			missing = append(missing, DiffMismatch{
				Field: "handshake.headers." + name, Expected: refVal, Reason: "candidate missing header", Got: "",
			})
			continue
		}
		if refVal != candVal {
			missing = append(missing, DiffMismatch{
				Field: "handshake.headers." + name, Expected: refVal, Got: candVal, Reason: "header value drift",
			})
		}
	}
	for name, candVal := range candMap {
		if _, ok := refMap[name]; !ok {
			extra = append(extra, DiffMismatch{
				Field: "handshake.headers." + name, Got: candVal, Reason: "candidate has extra header", Expected: "",
			})
		}
	}
	sortByField := func(m []DiffMismatch) { sort.Slice(m, func(i, j int) bool { return m[i].Field < m[j].Field }) }
	sortByField(missing)
	sortByField(extra)
	return missing, extra
}

// SummaryString returns a human-readable summary of a DiffReport,
// suitable for log output and CI failure messages.
func (r DiffReport) SummaryString() string {
	if !r.HasDiverged() {
		return "snapshot parity: clean"
	}
	var b strings.Builder
	b.WriteString("snapshot parity: divergence\n")
	for _, m := range r.Mismatches {
		b.WriteString("  mismatch ")
		b.WriteString(m.Field)
		b.WriteString(": expected=")
		b.WriteString(m.Expected)
		b.WriteString(" got=")
		b.WriteString(m.Got)
		b.WriteString(" (")
		b.WriteString(m.Reason)
		b.WriteString(")\n")
	}
	for _, m := range r.Missing {
		b.WriteString("  missing ")
		b.WriteString(m.Field)
		b.WriteString(": expected=")
		b.WriteString(m.Expected)
		b.WriteString(" (")
		b.WriteString(m.Reason)
		b.WriteString(")\n")
	}
	for _, m := range r.Extra {
		b.WriteString("  extra ")
		b.WriteString(m.Field)
		b.WriteString(": got=")
		b.WriteString(m.Got)
		b.WriteString(" (")
		b.WriteString(m.Reason)
		b.WriteString(")\n")
	}
	return b.String()
}
