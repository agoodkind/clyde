package mitm

import (
	"fmt"
	"sort"
	"strings"
)

// DiffReport is the structured output of comparing two Snapshot
// values. Empty Mismatches/Missing/Extra/FlavorReports means the two
// snapshots are equivalent under the snapshot contract.
type DiffReport struct {
	Upstream       string             `json:"upstream"`
	MissingFlavors []string           `json:"missing_flavors,omitempty"`
	ExtraFlavors   []string           `json:"extra_flavors,omitempty"`
	FlavorReports  []FlavorDiffReport `json:"flavor_reports,omitempty"`
}

// FlavorDiffReport captures divergence for one flavor that exists
// in both reference and candidate.
type FlavorDiffReport struct {
	Slug                  string         `json:"slug"`
	HeaderMissing         []string       `json:"header_missing,omitempty"`
	HeaderExtra           []string       `json:"header_extra,omitempty"`
	HeaderClassChanged    []DiffMismatch `json:"header_class_changed,omitempty"`
	HeaderValuesDiff      []DiffMismatch `json:"header_values_diff,omitempty"`
	BodyMissing           []string       `json:"body_missing,omitempty"`
	BodyExtra             []string       `json:"body_extra,omitempty"`
	BetaFingerprintChange *DiffMismatch  `json:"beta_fingerprint_change,omitempty"`
	UserAgentChange       *DiffMismatch  `json:"user_agent_change,omitempty"`
}

// HasDiverged reports whether any flavor or top-level shape diverged.
func (r DiffReport) HasDiverged() bool {
	if len(r.MissingFlavors) > 0 || len(r.ExtraFlavors) > 0 {
		return true
	}
	for _, fr := range r.FlavorReports {
		if fr.HasDiverged() {
			return true
		}
	}
	return false
}

// HasDiverged reports per-flavor divergence.
func (r FlavorDiffReport) HasDiverged() bool {
	return len(r.HeaderMissing) > 0 ||
		len(r.HeaderExtra) > 0 ||
		len(r.HeaderClassChanged) > 0 ||
		len(r.HeaderValuesDiff) > 0 ||
		len(r.BodyMissing) > 0 ||
		len(r.BodyExtra) > 0 ||
		r.BetaFingerprintChange != nil ||
		r.UserAgentChange != nil
}

// SummaryString renders a compact human-readable summary suitable
// for cron logs and CLI output.
func (r DiffReport) SummaryString() string {
	if !r.HasDiverged() {
		return "wire shape OK for upstream=" + r.Upstream
	}
	var b strings.Builder
	fmt.Fprintf(&b, "wire shape DRIFT for upstream=%s\n", r.Upstream)
	if len(r.MissingFlavors) > 0 {
		fmt.Fprintf(&b, "  missing flavors: %s\n", strings.Join(r.MissingFlavors, ", "))
	}
	if len(r.ExtraFlavors) > 0 {
		fmt.Fprintf(&b, "  extra flavors:   %s\n", strings.Join(r.ExtraFlavors, ", "))
	}
	for _, fr := range r.FlavorReports {
		if !fr.HasDiverged() {
			continue
		}
		fmt.Fprintf(&b, "  flavor %s\n", fr.Slug)
		if fr.UserAgentChange != nil {
			fmt.Fprintf(&b, "    user-agent: %s -> %s\n", fr.UserAgentChange.Expected, fr.UserAgentChange.Got)
		}
		if fr.BetaFingerprintChange != nil {
			fmt.Fprintf(&b, "    beta fingerprint changed\n")
		}
		if len(fr.HeaderMissing) > 0 {
			fmt.Fprintf(&b, "    header missing: %s\n", strings.Join(fr.HeaderMissing, ", "))
		}
		if len(fr.HeaderExtra) > 0 {
			fmt.Fprintf(&b, "    header extra:   %s\n", strings.Join(fr.HeaderExtra, ", "))
		}
		if len(fr.HeaderClassChanged) > 0 {
			for _, m := range fr.HeaderClassChanged {
				fmt.Fprintf(&b, "    header %s class %s -> %s\n", m.Field, m.Expected, m.Got)
			}
		}
		if len(fr.HeaderValuesDiff) > 0 {
			for _, m := range fr.HeaderValuesDiff {
				fmt.Fprintf(&b, "    header %s values: %s -> %s\n", m.Field, m.Expected, m.Got)
			}
		}
		if len(fr.BodyMissing) > 0 {
			fmt.Fprintf(&b, "    body missing: %s\n", strings.Join(fr.BodyMissing, ", "))
		}
		if len(fr.BodyExtra) > 0 {
			fmt.Fprintf(&b, "    body extra:   %s\n", strings.Join(fr.BodyExtra, ", "))
		}
	}
	return b.String()
}

// DiffSnapshots compares two snapshots. The first argument is the local
// baseline or explicit reference; the second is the candidate observed
// in the latest capture.
func DiffSnapshots(reference, candidate Snapshot) DiffReport {
	report := DiffReport{Upstream: reference.Upstream.Name, MissingFlavors: nil, ExtraFlavors: nil, FlavorReports: nil}

	refFlavors := make(map[string]FlavorShape, len(reference.Flavors))
	for _, fl := range reference.Flavors {
		refFlavors[fl.Slug] = fl
	}
	candFlavors := make(map[string]FlavorShape, len(candidate.Flavors))
	for _, fl := range candidate.Flavors {
		candFlavors[fl.Slug] = fl
	}

	for slug := range refFlavors {
		if _, ok := candFlavors[slug]; !ok {
			report.MissingFlavors = append(report.MissingFlavors, slug)
		}
	}
	for slug := range candFlavors {
		if _, ok := refFlavors[slug]; !ok {
			report.ExtraFlavors = append(report.ExtraFlavors, slug)
		}
	}
	sort.Strings(report.MissingFlavors)
	sort.Strings(report.ExtraFlavors)

	common := make([]string, 0, len(refFlavors))
	for slug := range refFlavors {
		if _, ok := candFlavors[slug]; ok {
			common = append(common, slug)
		}
	}
	sort.Strings(common)

	for _, slug := range common {
		fr := diffFlavor(refFlavors[slug], candFlavors[slug])
		if fr.HasDiverged() {
			report.FlavorReports = append(report.FlavorReports, fr)
		}
	}
	return report
}

func diffFlavor(ref, cand FlavorShape) FlavorDiffReport {
	out := FlavorDiffReport{Slug: ref.Slug, HeaderMissing: nil, HeaderExtra: nil, HeaderClassChanged: nil, HeaderValuesDiff: nil, BodyMissing: nil, BodyExtra: nil, BetaFingerprintChange: nil, UserAgentChange: nil}

	if ref.Signature.UserAgent != cand.Signature.UserAgent {
		out.UserAgentChange = &DiffMismatch{
			Field:    "user_agent",
			Expected: ref.Signature.UserAgent,
			Got:      cand.Signature.UserAgent,
			Reason:   "user agent changed",
		}
	}
	if ref.Signature.BetaFingerprint != cand.Signature.BetaFingerprint {
		out.BetaFingerprintChange = &DiffMismatch{
			Field:    "beta_fingerprint",
			Expected: ref.Signature.BetaFingerprint,
			Got:      cand.Signature.BetaFingerprint,
			Reason:   "beta fingerprint changed",
		}
	}

	refHeaders := indexHeaders(ref.Headers)
	candHeaders := indexHeaders(cand.Headers)
	for name, refHdr := range refHeaders {
		candHdr, ok := candHeaders[name]
		if !ok {
			out.HeaderMissing = append(out.HeaderMissing, name)
			continue
		}
		if refHdr.Classification != candHdr.Classification {
			out.HeaderClassChanged = append(out.HeaderClassChanged, DiffMismatch{
				Field:    name,
				Expected: string(refHdr.Classification),
				Got:      string(candHdr.Classification),
				Reason:   "header classification changed",
			})
		}
		if refHdr.Classification == HeaderClassConstant || refHdr.Classification == HeaderClassEnum {
			if !stringSlicesEqual(refHdr.ObservedValues, candHdr.ObservedValues) {
				out.HeaderValuesDiff = append(out.HeaderValuesDiff, DiffMismatch{
					Field:    name,
					Expected: strings.Join(refHdr.ObservedValues, "|"),
					Got:      strings.Join(candHdr.ObservedValues, "|"),
					Reason:   "header observed values changed",
				})
			}
		}
	}
	for name := range candHeaders {
		if _, ok := refHeaders[name]; !ok {
			out.HeaderExtra = append(out.HeaderExtra, name)
		}
	}
	sort.Strings(out.HeaderMissing)
	sort.Strings(out.HeaderExtra)

	refKeys := stringSet(ref.Signature.BodyKeys)
	candKeys := stringSet(cand.Signature.BodyKeys)
	for k := range refKeys {
		if !candKeys[k] {
			out.BodyMissing = append(out.BodyMissing, k)
		}
	}
	for k := range candKeys {
		if !refKeys[k] {
			out.BodyExtra = append(out.BodyExtra, k)
		}
	}
	sort.Strings(out.BodyMissing)
	sort.Strings(out.BodyExtra)
	return out
}

func indexHeaders(hs []Header) map[string]Header {
	out := make(map[string]Header, len(hs))
	for _, h := range hs {
		out[strings.ToLower(h.Name)] = h
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSet(s []string) map[string]bool {
	out := make(map[string]bool, len(s))
	for _, v := range s {
		out[v] = true
	}
	return out
}
