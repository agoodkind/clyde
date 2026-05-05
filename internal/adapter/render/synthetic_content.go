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
//
// Ref carries the optional data-ref attribute parsed off the open marker.
// For Codex thinking parts, the renderer emits the upstream item id (e.g.
// "rs_abc123") as data-ref so the inbound mapper can correlate the round-
// tripped envelope with provider state. Empty for parts produced by
// markers without a data-ref attribute (the legacy shape).
//
// Encrypted carries the optional data-encrypted attribute parsed off the
// CLOSE marker. For Codex thinking parts, this is the opaque server-signed
// `encrypted_content` blob the upstream attaches to the matching Reasoning
// item. The blob rides inline on the close marker so Cursor's transcript
// owns persistence; on the next turn the inbound mapper attaches it to the
// round-tripped Reasoning item directly. Empty for parts produced by
// markers without a data-encrypted attribute (legacy and Anthropic spans).
type SyntheticPart struct {
	Kind      SyntheticContentKind
	Body      string
	Ref       string
	Encrypted string
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

// dataRefAttrPattern is the optional `data-ref="..."` attribute fragment that
// may appear inside an open marker. The attribute name is fixed; the value
// allows any character except a literal double-quote so simple HTML escaping
// is unnecessary at our use sites (callers pass opaque ids like rs_abc123).
const dataRefAttrPattern = `(?: data-ref="([^"]*)")?`

// dataEncryptedAttrPattern is the optional `data-encrypted="..."` attribute
// fragment that may appear on the CLOSE marker. The blob is base64 so
// `[^"]*` is a safe match. For forward compatibility the open marker
// regex also accepts (but ignores) a `data-encrypted` attribute.
const dataEncryptedAttrPattern = `(?: data-encrypted="([^"]*)")?`

func init() {
	for _, spec := range syntheticContentSpecs {
		marker := regexp.QuoteMeta(spec.Marker)
		// stripRE accepts the open marker with or without a data-ref
		// attribute and the close marker with or without a
		// data-encrypted attribute. Non-capturing groups keep the
		// match anchored so we still consume the entire envelope on
		// strip.
		spec.stripRE = regexp.MustCompile(
			`(?s)<!--` + marker + `(?: data-ref="[^"]*")?(?: data-encrypted="[^"]*")?-->` +
				`.*?` +
				`<!--/` + marker + `(?: data-encrypted="[^"]*")?-->\s*`,
		)
		// captureRE submatches (1) the optional open data-ref value,
		// (2) the inner body, and (3) the optional close
		// data-encrypted value. Forward compat: an encrypted attribute
		// on the open marker is accepted but ignored (the close is the
		// authoritative carrier because encrypted_content arrives at
		// response.output_item.done after the open marker has already
		// shipped to Cursor). The trailing `\s*` mirrors stripRE so
		// the two paths agree on where one part ends and the next
		// begins.
		spec.captureRE = regexp.MustCompile(
			`(?s)<!--` + marker + dataRefAttrPattern + `(?: data-encrypted="[^"]*")?-->` +
				`(.*?)` +
				`<!--/` + marker + dataEncryptedAttrPattern + `-->\s*`,
		)
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
// the requested synthetic block kind. The marker carries no attributes; for
// the data-ref-tagged variant use [SyntheticContentOpenWithRef].
func SyntheticContentOpen(kind SyntheticContentKind) string {
	return SyntheticContentOpenWithRef(kind, "")
}

// SyntheticContentOpenWithRef returns the leading marker plus the visible
// header for the requested synthetic block kind, optionally annotated with a
// data-ref attribute. The ref is emitted as `<!--<marker> data-ref="<ref>"-->`
// when non-empty so the inbound mapper can correlate the round-tripped
// envelope with provider state (e.g. a stored Codex encrypted_content blob).
//
// Empty ref matches the legacy [SyntheticContentOpen] shape exactly so
// existing parsers and tests stay valid.
//
// The ref must not contain a literal double-quote; callers using upstream
// item ids (alphanumeric plus hyphen and underscore) are safe by construction.
func SyntheticContentOpenWithRef(kind SyntheticContentKind, ref string) string {
	spec := specFor(kind)
	if spec == nil {
		return ""
	}
	openTag := "<!--" + spec.Marker
	if ref != "" {
		openTag += ` data-ref="` + ref + `"`
	}
	openTag += "-->"
	return openTag + "\n" + spec.Header
}

// SyntheticContentClose returns the trailing marker for the requested
// synthetic block kind. It always ends with a blank line so subsequent
// markdown renders cleanly.
func SyntheticContentClose(kind SyntheticContentKind) string {
	return SyntheticContentCloseWithEncrypted(kind, "")
}

// SyntheticContentCloseWithEncrypted returns the trailing marker for the
// requested synthetic block kind, optionally annotated with a
// `data-encrypted` attribute carrying an opaque server-signed blob. The
// attribute is emitted as `<!--/<marker> data-encrypted="<encrypted>"-->`
// when non-empty so the next-turn inbound mapper can recover the blob
// without consulting an external store. Empty encrypted matches the
// legacy [SyntheticContentClose] shape exactly.
//
// The encrypted value must not contain a literal double-quote; callers
// using base64 (alphanumeric plus `+/=`) are safe by construction.
func SyntheticContentCloseWithEncrypted(kind SyntheticContentKind, encrypted string) string {
	spec := specFor(kind)
	if spec == nil {
		return ""
	}
	closeTag := "<!--/" + spec.Marker
	if encrypted != "" {
		closeTag += ` data-encrypted="` + encrypted + `"`
	}
	closeTag += "-->"
	return "\n" + closeTag + "\n\n"
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

// FormatSyntheticContentDeltaWithRef formats a streaming delta for the
// given kind, optionally annotating the open marker with a data-ref
// attribute.
//
// When open is true the leading marker, header, and a fresh blockquote
// prefix are included so the very first delta starts a new quoted block.
// When open is false the renderer is mid-stream inside an already-open
// block and the body's own newlines drive new quoted lines via "\n" ->
// "\n> " replacement, matching the existing reasoning streaming layout.
//
// The ref is only honored when open is true; mid-stream deltas never
// carry the attribute since the marker is already on the wire. Empty
// ref produces the legacy attribute-less shape.
func FormatSyntheticContentDeltaWithRef(kind SyntheticContentKind, open bool, ref, body string) string {
	spec := specFor(kind)
	if spec == nil {
		return body
	}
	decorated := formatSyntheticBody(spec, body, open)
	if open {
		return SyntheticContentOpenWithRef(kind, ref) + decorated
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
// caller passed to [FormatSyntheticContent] / [FormatSyntheticContentDeltaWithRef].
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
	kind      SyntheticContentKind
	start     int
	end       int
	bodyTrim  string
	ref       string
	encrypted string
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
		// Match the open prefix without committing to a specific tail
		// (the marker may carry an optional `data-ref="..."` attribute
		// before the closing `-->`). The captureRE is the authoritative
		// matcher; this Contains is just a cheap skip for plain text.
		if !strings.Contains(text, "<!--"+spec.Marker) {
			continue
		}
		idxs := spec.captureRE.FindAllStringSubmatchIndex(text, -1)
		for _, idx := range idxs {
			outerStart, outerEnd := idx[0], idx[1]
			refStart, refEnd := idx[2], idx[3]
			innerStart, innerEnd := idx[4], idx[5]
			encStart, encEnd := idx[6], idx[7]
			ref := ""
			if refStart >= 0 && refEnd >= 0 {
				ref = text[refStart:refEnd]
			}
			encrypted := ""
			if encStart >= 0 && encEnd >= 0 {
				encrypted = text[encStart:encEnd]
			}
			matches = append(matches, syntheticMatch{
				kind:      kind,
				start:     outerStart,
				end:       outerEnd,
				bodyTrim:  stripDecoration(spec, text[innerStart:innerEnd]),
				ref:       ref,
				encrypted: encrypted,
			})
		}
	}
	if len(matches) == 0 {
		return []SyntheticPart{{Kind: SyntheticKindText, Body: text, Ref: "", Encrypted: ""}}
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
		parts = append(parts, SyntheticPart{Kind: m.kind, Body: m.bodyTrim, Ref: m.ref, Encrypted: m.encrypted})
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
	return append(parts, SyntheticPart{Kind: SyntheticKindText, Body: text, Ref: "", Encrypted: ""})
}

// MaterializationStrategy picks how round-tripped synthetic envelope content
// is shaped before forwarding upstream. The exact same string values are
// declared in [internal/config.SyntheticInboundMaterialization]; provider
// mappers convert from the config type at the call site so the render
// package stays free of the config dependency.
type MaterializationStrategy string

// Materialization strategies. Values are stable enum strings shared with the
// config package contract.
const (
	// MaterializeNativeThinkingBlock asks the provider mapper to emit a
	// native upstream thinking content block per round-tripped thinking
	// part. Only Anthropic supports this in current code; Codex callers
	// must not request it.
	MaterializeNativeThinkingBlock MaterializationStrategy = "native_thinking_block"
	// MaterializePlainTextConcat folds round-tripped thinking bodies into
	// the assistant text block as plain prose (decoration stripped). Use
	// this when the upstream cannot accept native thinking blocks but the
	// reasoning trace is still wanted in context.
	MaterializePlainTextConcat MaterializationStrategy = "plain_text_concat"
	// MaterializeDrop discards thinking bodies before forwarding upstream.
	// The Codex default since Codex upstream cannot accept thinking blocks
	// and the trace bloats context.
	MaterializeDrop MaterializationStrategy = "drop"
	// MaterializePassthrough leaves the marker-wrapped envelope intact in
	// the assistant text block. Used by passthrough overrides where the
	// upstream is expected to accept Clyde's marker convention as-is.
	MaterializePassthrough MaterializationStrategy = "passthrough"
)

// MaterializedKind is the typed instruction the provider mapper consumes per
// output part of [MaterializeSyntheticParts].
type MaterializedKind string

// Materialized kinds.
const (
	// MaterializedKindText asks the mapper to emit a plain text content
	// block carrying Body verbatim.
	MaterializedKindText MaterializedKind = "text"
	// MaterializedKindNativeThinking asks the mapper to emit an
	// upstream-native thinking content block carrying Body verbatim.
	MaterializedKindNativeThinking MaterializedKind = "native_thinking"
)

// MaterializedPart is one ordered output instruction from
// [MaterializeSyntheticParts]. The provider mapper renders each part
// mechanically into its own upstream-native content block type.
type MaterializedPart struct {
	Kind MaterializedKind
	Body string
}

// MaterializeSyntheticParts applies a [MaterializationStrategy] to a
// pre-extracted [SyntheticPart] slice and returns the ordered output
// instructions for the provider mapper. Strategy decides what happens to
// SyntheticReasoning parts; SyntheticNotice parts are always dropped (notice
// envelopes are user-facing UI annotations, never forwarded upstream);
// SyntheticKindText parts are always emitted as MaterializedKindText.
//
// Implementation notes per strategy:
//   - MaterializeNativeThinkingBlock: each Reasoning part becomes a separate
//     MaterializedKindNativeThinking part. Order with surrounding text is
//     preserved.
//   - MaterializePlainTextConcat: Reasoning bodies are appended as plain
//     text, joined to surrounding text parts so the mapper can emit a single
//     contiguous text block when convenient. The materializer returns
//     separate text parts to keep the choice with the mapper.
//   - MaterializeDrop: Reasoning parts are dropped entirely.
//   - MaterializePassthrough: Reasoning parts are re-wrapped in their
//     original envelope and emitted as text, so the upstream sees the
//     marker convention verbatim.
//
// MaterializeSyntheticParts never returns nil for a non-empty input; an
// input with only dropped parts returns an empty slice.
func MaterializeSyntheticParts(parts []SyntheticPart, strategy MaterializationStrategy) []MaterializedPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]MaterializedPart, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case SyntheticKindText:
			if strings.TrimSpace(p.Body) == "" {
				continue
			}
			out = append(out, MaterializedPart{Kind: MaterializedKindText, Body: p.Body})
		case SyntheticReasoning:
			out = append(out, materializeReasoningPart(p.Body, strategy)...)
		case SyntheticNotice:
			continue
		}
	}
	return out
}

func materializeReasoningPart(body string, strategy MaterializationStrategy) []MaterializedPart {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil
	}
	switch strategy {
	case MaterializeNativeThinkingBlock:
		return []MaterializedPart{{Kind: MaterializedKindNativeThinking, Body: trimmed}}
	case MaterializePlainTextConcat:
		return []MaterializedPart{{Kind: MaterializedKindText, Body: trimmed}}
	case MaterializePassthrough:
		envelope := FormatSyntheticContent(SyntheticReasoning, trimmed)
		if envelope == "" {
			return nil
		}
		return []MaterializedPart{{Kind: MaterializedKindText, Body: envelope}}
	case MaterializeDrop:
		return nil
	}
	// Unknown strategy: behave like Drop so a future enum value never silently
	// leaks raw thinking content upstream.
	return nil
}
