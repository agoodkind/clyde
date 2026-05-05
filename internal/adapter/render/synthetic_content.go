// Package render owns the single source of truth for synthetic content
// envelopes that Clyde injects into Cursor's BYOK chat surface.
//
// Cursor's custom-OpenAI ingress reliably renders delta.content but does not
// honor secondary fields like reasoning_content for BYOK streams. To get the
// same visible affordances as Cursor's first-party providers (collapsible
// reasoning blocks, transient quota notices) we wrap synthetic UI bodies in
// HTML-comment marker pairs and emit them as ordinary delta.content. Every
// surface that emits synthetic content uses [FormatSyntheticContent], and
// every backend that needs to consume these envelopes before reusing the
// transcript upstream uses [ExtractSyntheticParts] and decides per kind
// whether to drop, materialize as a native upstream block, or keep as
// plain text. Adding a new synthetic block is a single entry in
// [syntheticContentSpecs].
package render

import (
	"regexp"
	"strings"
)

// SyntheticContentKind identifies a marker-wrapped synthetic block kind.
type SyntheticContentKind string

const (
	// SyntheticKindText is the kind assigned to the non-envelope segments of a
	// piece of assistant content. It is never emitted by Format* helpers; it
	// only appears in [ExtractSyntheticParts] output so consumers can tell
	// envelope bodies apart from prose without re-parsing.
	SyntheticKindText SyntheticContentKind = "text"
	// SyntheticReasoning (alias SyntheticKindThinking) wraps Cursor-visible
	// reasoning bodies emitted to delta.content alongside
	// delta.reasoning_content.
	SyntheticReasoning SyntheticContentKind = "thinking"
	// SyntheticKindThinking is the canonical kind name for round-tripped
	// reasoning content. It is the same value as [SyntheticReasoning] and
	// exists so consumers can switch on a "kind" enum without referencing
	// the older alias.
	SyntheticKindThinking = SyntheticReasoning
	// SyntheticNotice (alias SyntheticKindNotice) wraps transient quota and
	// runtime notices emitted to delta.content so they render as warning
	// blockquotes in Cursor BYOK.
	SyntheticNotice SyntheticContentKind = "notice"
	// SyntheticKindNotice is the canonical kind name for notice content.
	SyntheticKindNotice = SyntheticNotice
)

// SyntheticPart is one ordered segment of an assistant content string after
// envelope detection. Body carries the raw upstream-ready text with all
// envelope decoration (markers, header line, blockquote prefixes) stripped.
type SyntheticPart struct {
	Kind SyntheticContentKind
	Body string
}

// syntheticContentSpec describes the rendering and stripping rules for one
// synthetic content kind. Header is the visible block header rendered inside
// the open marker; QuotePrefix means each body line is rendered as a markdown
// blockquote line.
type syntheticContentSpec struct {
	Marker      string
	Header      string
	QuotePrefix bool
	stripRE     *regexp.Regexp
	captureRE   *regexp.Regexp
}

var syntheticContentSpecs = map[SyntheticContentKind]*syntheticContentSpec{
	SyntheticReasoning: {
		Marker:      "clyde-thinking",
		Header:      "> **💭 Thinking...**\n> \n",
		QuotePrefix: true,
	},
	SyntheticNotice: {
		Marker:      "clyde-notice",
		Header:      "",
		QuotePrefix: true,
	},
}

// orderedSyntheticKinds lists the kinds in deterministic order so extraction
// is reproducible across runs (Go map iteration is not).
var orderedSyntheticKinds = []SyntheticContentKind{SyntheticReasoning, SyntheticNotice}

func init() {
	for _, spec := range syntheticContentSpecs {
		spec.stripRE = regexp.MustCompile(`(?s)<!--` + regexp.QuoteMeta(spec.Marker) + `-->.*?<!--/` + regexp.QuoteMeta(spec.Marker) + `-->\s*`)
		// captureRE matches the envelope and submatches the inner body.
		// The trailing `\s*` mirrors stripRE so the strip path (which
		// consumes whitespace between an envelope close and the next
		// segment) and the extract path agree on where one part ends
		// and the next begins.
		spec.captureRE = regexp.MustCompile(`(?s)<!--` + regexp.QuoteMeta(spec.Marker) + `-->(.*?)<!--/` + regexp.QuoteMeta(spec.Marker) + `-->\s*`)
	}
}

func specFor(kind SyntheticContentKind) *syntheticContentSpec {
	spec, ok := syntheticContentSpecs[kind]
	if !ok {
		return nil
	}
	return spec
}

// SyntheticContentOpen returns the leading marker plus the visible header for
// the requested synthetic block kind.
func SyntheticContentOpen(kind SyntheticContentKind) string {
	spec := specFor(kind)
	if spec == nil {
		return ""
	}
	return "<!--" + spec.Marker + "-->\n" + spec.Header
}

// SyntheticContentClose returns the trailing marker for the requested
// synthetic block kind. It always ends with a blank line so subsequent
// markdown renders cleanly.
func SyntheticContentClose(kind SyntheticContentKind) string {
	spec := specFor(kind)
	if spec == nil {
		return ""
	}
	return "\n<!--/" + spec.Marker + "-->\n\n"
}

// FormatSyntheticContent wraps body in a complete marker-enclosed block. body
// may contain newlines; if the kind uses QuotePrefix each line is rendered as
// a markdown blockquote line so Cursor displays it as a visible aside. This
// is the one-shot formatter used for non-streaming notice injection and tests.
func FormatSyntheticContent(kind SyntheticContentKind, body string) string {
	spec := specFor(kind)
	if spec == nil {
		return body
	}
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	return SyntheticContentOpen(kind) + formatSyntheticBody(spec, body, true) + SyntheticContentClose(kind)
}

// FormatSyntheticContentDelta formats a streaming delta for the given kind.
//
// When open is true the leading marker, header, and a fresh blockquote prefix
// are included so the very first delta starts a new quoted block. When open
// is false the renderer is mid-stream inside an already-open block and the
// body's own newlines drive new quoted lines via "\n" -> "\n> " replacement,
// matching the existing reasoning streaming layout.
func FormatSyntheticContentDelta(kind SyntheticContentKind, open bool, body string) string {
	spec := specFor(kind)
	if spec == nil {
		return body
	}
	decorated := formatSyntheticBody(spec, body, open)
	if open {
		return SyntheticContentOpen(kind) + decorated
	}
	return decorated
}

func formatSyntheticBody(spec *syntheticContentSpec, body string, leadingPrefix bool) string {
	if !spec.QuotePrefix {
		return body
	}
	quoted := strings.ReplaceAll(body, "\n", "\n> ")
	if leadingPrefix {
		return "> " + quoted
	}
	return quoted
}

// stripDecoration reverses formatSyntheticBody: it strips the leading header
// (if any) and the blockquote prefix, returning the raw body text the original
// caller passed to [FormatSyntheticContent] / [FormatSyntheticContentDelta].
//
// The decorated input begins immediately after the open marker and ends
// immediately before the close marker (i.e. the captureRE submatch).
func stripDecoration(spec *syntheticContentSpec, decorated string) string {
	body := decorated
	// Capture body sometimes begins with a leading newline because
	// SyntheticContentOpen ends with "\n" before the header. Drop one
	// leading newline if present so the header match aligns.
	body = strings.TrimPrefix(body, "\n")
	if spec.Header != "" {
		body = strings.TrimPrefix(body, spec.Header)
	}
	if spec.QuotePrefix {
		// SyntheticContentClose starts with "\n" before the close marker;
		// drop trailing whitespace so blockquote-line reversal sees clean
		// terminator.
		body = strings.TrimRight(body, "\n")
		// Reverse "\n> " -> "\n" and the leading "> ".
		body = strings.TrimPrefix(body, "> ")
		body = strings.ReplaceAll(body, "\n> ", "\n")
		// A line that was just "> " in the source becomes "" after the
		// prefix is removed; that is fine because it represents an empty
		// line in the original body.
	}
	return body
}

// syntheticMatch is one envelope match found inside an assistant content
// string. It carries enough context to splice the raw Body into a typed part
// without re-parsing.
type syntheticMatch struct {
	kind     SyntheticContentKind
	start    int
	end      int
	bodyTrim string
}

// ExtractSyntheticParts parses text and returns the ordered list of parts.
// Envelope segments produce [SyntheticPart] entries with the matching kind and
// the raw upstream-ready Body (decoration stripped). Non-envelope text
// segments produce [SyntheticKindText] parts. An empty input returns nil.
//
// Consecutive envelope blocks of the same kind produce separate parts;
// callers that need to merge them can do so explicitly.
func ExtractSyntheticParts(text string) []SyntheticPart {
	if text == "" {
		return nil
	}
	var matches []syntheticMatch
	for _, kind := range orderedSyntheticKinds {
		spec := syntheticContentSpecs[kind]
		if !strings.Contains(text, "<!--"+spec.Marker+"-->") {
			continue
		}
		idxs := spec.captureRE.FindAllStringSubmatchIndex(text, -1)
		for _, idx := range idxs {
			outerStart, outerEnd := idx[0], idx[1]
			innerStart, innerEnd := idx[2], idx[3]
			matches = append(matches, syntheticMatch{
				kind:     kind,
				start:    outerStart,
				end:      outerEnd,
				bodyTrim: stripDecoration(spec, text[innerStart:innerEnd]),
			})
		}
	}
	if len(matches) == 0 {
		return []SyntheticPart{{Kind: SyntheticKindText, Body: text}}
	}
	// Insertion sort by start offset; tiny N (matches per assistant turn).
	for i := 1; i < len(matches); i++ {
		j := i
		for j > 0 && matches[j-1].start > matches[j].start {
			matches[j-1], matches[j] = matches[j], matches[j-1]
			j--
		}
	}

	var parts []SyntheticPart
	cursor := 0
	for _, m := range matches {
		if m.start < cursor {
			// Overlapping (should not happen with non-greedy regex);
			// skip to preserve linear order.
			continue
		}
		if m.start > cursor {
			parts = appendTextPart(parts, text[cursor:m.start])
		}
		parts = append(parts, SyntheticPart{Kind: m.kind, Body: m.bodyTrim})
		cursor = m.end
	}
	if cursor < len(text) {
		parts = appendTextPart(parts, text[cursor:])
	}
	return parts
}

func appendTextPart(parts []SyntheticPart, text string) []SyntheticPart {
	if text == "" {
		return parts
	}
	// Drop pure-whitespace gap segments introduced by SyntheticContentClose
	// trailing "\n\n" + the next envelope's leading newline. Preserve
	// non-whitespace text verbatim so user prose is untouched.
	if strings.TrimSpace(text) == "" {
		return parts
	}
	return append(parts, SyntheticPart{Kind: SyntheticKindText, Body: text})
}
