package render

import (
	"regexp"
	"strings"
)

// SyntheticContentKind identifies a marker-wrapped synthetic block kind.
type SyntheticContentKind string

// SyntheticOrigin identifies which model family produced a synthetic
// thinking piece. It is parsed off the open marker's `data-origin`
// attribute on the next turn so the receiving adapter can decide between
// replaying the piece in its own native form (same-family) or injecting
// the body as plain text (foreign or unknown family).
//
// Empty / absent `data-origin` on the open marker resolves to
// [OriginUnknown], which routes through the same plain-text injection
// path the foreign-origin case uses.
type SyntheticOrigin string

const (
	// OriginUnknown is the resolved origin when the open marker carries
	// no `data-origin` attribute. Pre-upgrade transcripts always resolve
	// here, and the receiving adapter treats them like foreign-origin
	// pieces (plain-text injection).
	OriginUnknown SyntheticOrigin = ""
	// OriginAnthropic tags a thinking piece produced by an Anthropic
	// stream. The Anthropic adapter replays it natively as a
	// [ThinkingBlock]; other adapters inject it as plain text.
	OriginAnthropic SyntheticOrigin = "anthropic"
	// OriginCodex tags a thinking piece produced by a Codex stream. The
	// Codex adapter replays it natively as a reasoning input item; other
	// adapters inject it as plain text.
	OriginCodex SyntheticOrigin = "codex"
)

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
	// SyntheticRedactedThinking wraps Anthropic redacted_thinking content
	// blocks. The upstream emits these for thinking content the API will
	// not surface in cleartext; the wire payload is an opaque base64 data
	// blob carried on the close marker as a `data-encrypted` attribute,
	// not text. The body has no human-readable contents and is rendered
	// to Cursor as a fixed `[redacted thinking]` placeholder so the user
	// sees the block exists without exposing model internals. Kept
	// separate from [SyntheticReasoning] so the typed enum is the source
	// of truth and the materializer can route the opaque blob to a
	// dedicated upstream block type without conflating semantics.
	SyntheticRedactedThinking SyntheticContentKind = "redacted_thinking"
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
//
// Signature carries the optional data-signature attribute parsed off the
// CLOSE marker. For Anthropic thinking parts, this is the opaque per-block
// signature the upstream emits via the `signature_delta` SSE event. The
// value rides inline on the close marker as a sibling of data-encrypted so
// Cursor's transcript owns persistence; on the next turn the Anthropic
// mapper copies it onto the materialized native thinking block so
// signature validation passes upstream. Empty for parts produced by
// markers without a data-signature attribute (legacy and Codex spans).
type SyntheticPart struct {
	Kind      SyntheticContentKind
	Body      string
	Ref       string
	Encrypted string
	Signature string
	// Origin identifies the model family that produced this thinking
	// piece, parsed off the open marker's `data-origin` attribute. The
	// receiving adapter uses it to decide between native replay (same
	// family) and plain-text injection (foreign or unknown family).
	// Empty for non-thinking parts and for pre-upgrade transcripts.
	Origin SyntheticOrigin
}

// syntheticContentSpec describes the rendering and stripping rules for one
// synthetic content kind. Header is the visible block header rendered inside
// the open marker; QuotePrefix means each body line is rendered as a markdown
// blockquote line.
type syntheticContentSpec struct {
	Marker      string
	Header      string
	QuotePrefix bool
	// openOnlyRE matches a bare open marker (with its optional data-ref /
	// data-origin attributes), independent of what follows. closeOnlyRE
	// matches a bare close marker (with its optional data-encrypted /
	// data-signature attributes), independent of what precedes it.
	// pairKindMatches scans both and pairs them sequentially rather than
	// matching a single open-through-close regex across the whole text:
	// a regex written as open...(lazily-anything)...close pairs the FIRST
	// open with the NEXT close regardless of what lies between them,
	// which wrongly absorbs a real complete envelope (and everything
	// around it) into the span between an unrelated earlier mention of
	// the open marker and that envelope's own close.
	openOnlyRE  *regexp.Regexp
	closeOnlyRE *regexp.Regexp
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
	SyntheticRedactedThinking: {
		Marker:      "clyde-redacted-thinking",
		Header:      "> **[redacted thinking]**\n> \n",
		QuotePrefix: true,
	},
}

// orderedSyntheticKinds lists the kinds in deterministic order so extraction
// is reproducible across runs (Go map iteration is not).
var orderedSyntheticKinds = []SyntheticContentKind{SyntheticReasoning, SyntheticNotice, SyntheticRedactedThinking}

// dataRefAttrPattern is the optional `data-ref="..."` attribute fragment that
// may appear inside an open marker. The attribute name is fixed; the value
// allows any character except a literal double-quote so simple HTML escaping
// is unnecessary at our use sites (callers pass opaque ids like rs_abc123).
const dataRefAttrPattern = `(?: data-ref="([^"]*)")?`

// dataOriginAttrPattern is the optional `data-origin="..."` attribute
// fragment that may appear inside an open marker. Values are typed strings
// matching [SyntheticOrigin] members (e.g. "anthropic", "codex"). Empty /
// absent resolves to [OriginUnknown].
const dataOriginAttrPattern = `(?: data-origin="([^"]*)")?`

// dataEncryptedAttrPattern is the optional `data-encrypted="..."` attribute
// fragment that may appear on the CLOSE marker. The blob is base64 so
// `[^"]*` is a safe match. Carries the Codex `encrypted_content` blob;
// also reused for redacted_thinking opaque payloads via the
// SyntheticRedactedThinking kind. Lives on the close marker because the
// blob is only known after the reasoning span finishes upstream.
const dataEncryptedAttrPattern = `(?: data-encrypted="([^"]*)")?`

// dataSignatureAttrPattern is the optional `data-signature="..."` attribute
// fragment that may appear on the CLOSE marker (sibling of
// data-encrypted; carries the Anthropic per-thinking-block signature).
// Lives on the close marker because the signature arrives via a late
// signature_delta SSE event after the open marker has already shipped.
// Value is base64 so `[^"]*` is a safe match.
const dataSignatureAttrPattern = `(?: data-signature="([^"]*)")?`

func init() {
	for _, spec := range syntheticContentSpecs {
		marker := regexp.QuoteMeta(spec.Marker)
		// data-ref and data-origin are known when the open marker is
		// emitted, so they ride on the open marker. data-encrypted
		// (codex encrypted_content) and data-signature (anthropic
		// per-thinking-block signature) arrive late on the wire, so they
		// ride on the close marker. openOnlyRE submatches (1) data-ref,
		// (2) data-origin; closeOnlyRE submatches (1) data-encrypted,
		// (2) data-signature.
		spec.openOnlyRE = regexp.MustCompile(
			`<!--` + marker + dataRefAttrPattern + dataOriginAttrPattern + `-->`,
		)
		spec.closeOnlyRE = regexp.MustCompile(
			`<!--/` + marker + dataEncryptedAttrPattern + dataSignatureAttrPattern + `-->`,
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
// the attribute-tagged variant use [SyntheticContentOpenWithRef].
func SyntheticContentOpen(kind SyntheticContentKind) string {
	return SyntheticContentOpenWithRef(kind, "", OriginUnknown)
}

// SyntheticContentOpenWithRef returns the leading marker plus the visible
// header for the requested synthetic block kind, annotated with the data-ref
// and data-origin attributes. Both are independently optional; an empty ref
// omits data-ref, and an [OriginUnknown] origin omits data-origin. The
// on-wire order is fixed (ref first, origin second) so openOnlyRE's
// submatch groups stay in that order.
//
// The ref correlates the round-tripped envelope with provider state (e.g. a
// stored Codex encrypted_content blob); the origin identifies the producing
// model family so the receiving adapter on the next turn can choose between
// native replay and plain-text injection.
//
// Neither value may contain a literal double-quote. Callers pass opaque ids
// (alphanumeric plus hyphen/underscore) and typed enum values, which are safe
// by construction.
func SyntheticContentOpenWithRef(kind SyntheticContentKind, ref string, origin SyntheticOrigin) string {
	spec := specFor(kind)
	if spec == nil {
		return ""
	}
	openTag := "<!--" + spec.Marker
	if ref != "" {
		openTag += ` data-ref="` + ref + `"`
	}
	if origin != OriginUnknown {
		openTag += ` data-origin="` + string(origin) + `"`
	}
	openTag += "-->"
	return openTag + "\n" + spec.Header
}

// SyntheticContentClose returns the trailing marker for the requested
// synthetic block kind. It always ends with a blank line so subsequent
// markdown renders cleanly.
func SyntheticContentClose(kind SyntheticContentKind) string {
	return SyntheticContentCloseWithAttrs(kind, "", "")
}

// SyntheticContentCloseWithAttrs returns the trailing marker for the
// requested synthetic block kind, optionally annotated with both a
// `data-encrypted` attribute (codex `encrypted_content` blob) and a
// `data-signature` attribute (Anthropic per-thinking-block signature).
// Each attribute is independently optional. The order on the wire is
// fixed: encrypted first, signature second, matching closeOnlyRE's
// submatch order, so each provider mapper reads only the attribute it
// owns.
//
// Both attributes ride on the CLOSE marker because they arrive late on the
// upstream wire (encrypted_content on response.output_item.done; signature
// via a signature_delta SSE event near the end of the thinking block). At
// the moment the open marker ships to Cursor, neither value is known yet.
//
// Neither value may contain a literal double-quote; callers using base64
// (alphanumeric plus `+/=`) are safe by construction. Empty values for
// both arguments match the legacy [SyntheticContentClose] shape exactly.
func SyntheticContentCloseWithAttrs(kind SyntheticContentKind, encrypted, signature string) string {
	spec := specFor(kind)
	if spec == nil {
		return ""
	}
	closeTag := "<!--/" + spec.Marker
	if encrypted != "" {
		closeTag += ` data-encrypted="` + encrypted + `"`
	}
	if signature != "" {
		closeTag += ` data-signature="` + signature + `"`
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

// FormatSyntheticContentDeltaWithRef formats a streaming delta for the given
// kind, annotating the open marker with the data-ref and data-origin
// attributes.
//
// open controls whether this delta emits the open marker and header. When
// true the leading marker and header are included; when false the renderer
// is mid-stream inside an already-open block.
//
// leadingQuote controls whether the body's first line is prefixed with a
// fresh `> `. It must be true whenever this delta carries the first body
// line of the block, even when open is false (the open marker shipped in a
// prior chunk via SyntheticContentOpenWithRef). When leadingQuote is false
// the body's own newlines drive new quoted lines via "\n" -> "\n> "
// replacement, matching the existing reasoning streaming layout.
//
// The ref and origin are only honored when open is true; mid-stream deltas
// never carry attributes since the open marker is already on the wire.
// Empty ref and [OriginUnknown] origin produce the attribute-less shape.
func FormatSyntheticContentDeltaWithRef(kind SyntheticContentKind, open, leadingQuote bool, ref string, origin SyntheticOrigin, body string) string {
	spec := specFor(kind)
	if spec == nil {
		return body
	}
	decorated := formatSyntheticBody(spec, body, leadingQuote)
	if open {
		return SyntheticContentOpenWithRef(kind, ref, origin) + decorated
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
// immediately before the close marker (i.e. the text a paired open and
// close marker enclose; see [pairKindMatches]).
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
	origin    SyntheticOrigin
	encrypted string
	signature string
}

// markerOccurrence is one open or close marker occurrence found while
// scanning text for one kind, carrying whatever attributes that half of
// the marker can carry (an open marker's data-ref/data-origin, or a close
// marker's data-encrypted/data-signature; the other pair is always empty).
type markerOccurrence struct {
	isOpen    bool
	start     int
	end       int
	ref       string
	origin    SyntheticOrigin
	encrypted string
	signature string
}

// ExtractSyntheticParts parses text and returns the ordered list of parts.
// Envelope segments produce [SyntheticPart] entries with the matching kind and
// the raw upstream-ready Body (decoration stripped). Non-envelope text
// segments produce [SyntheticKindText] parts. An empty input returns nil.
//
// A marker that cannot be matched as a complete open-and-close pair is left
// inside the surrounding [SyntheticKindText] part rather than treated as an
// envelope on its own: an unmatched marker is equally consistent with prose
// that quotes or discusses the marker syntax (ordinary traffic in a
// repository that builds this exact feature) as it is with a genuinely
// truncated envelope, and the two cannot be told apart from the marker
// alone. Only a complete pair is strong enough evidence that this is
// Clyde's own injected content. See [pairKindMatches] for how a pair is
// identified: proximity between an open and a later close is not enough,
// because a marker merely mentioned in prose before a real, unrelated
// envelope is not that envelope's open half.
//
// Consecutive envelope blocks of the same kind produce separate parts;
// callers that need to merge them can do so explicitly.
func ExtractSyntheticParts(text string) []SyntheticPart {
	if text == "" {
		return nil
	}
	matches := collectSyntheticMatches(text)
	if len(matches) == 0 {
		return []SyntheticPart{{Kind: SyntheticKindText, Body: text, Ref: "", Encrypted: "", Signature: "", Origin: OriginUnknown}}
	}
	sortSyntheticMatches(matches)
	return spliceSyntheticParts(text, matches)
}

// collectSyntheticMatches scans text for every registered marker kind and
// returns one [syntheticMatch] per complete open-and-close pair found. Order
// is per-kind grouped; the caller sorts before splicing.
func collectSyntheticMatches(text string) []syntheticMatch {
	var matches []syntheticMatch
	for _, kind := range orderedSyntheticKinds {
		spec := syntheticContentSpecs[kind]
		if !strings.Contains(text, "<!--"+spec.Marker) {
			continue
		}
		matches = append(matches, pairKindMatches(text, kind, spec)...)
	}
	return matches
}

// pairKindMatches finds every complete open-and-close pair for one kind by
// scanning marker occurrences (open and close) in text order and pairing
// each open with the very next marker event of that kind, but only when
// that event is a close.
//
// A single regex spanning open...(lazily-anything)...close cannot express
// this: it pairs the FIRST open it finds with the NEXT close anywhere
// after it, so an open marker merely mentioned in prose ("here's what the
// marker looks like: <!--clyde-thinking-->"), followed later by a real,
// unrelated complete envelope, gets wrongly paired with that envelope's
// close, and everything between the mention and the real close, including
// the real envelope's own open marker, is swallowed into one bogus match.
//
// This sequential scan instead tracks one pending open at a time: an open
// event supersedes whatever open was already pending (the superseded one
// never pairs with anything, so its literal marker text stays put as
// ordinary text) rather than waiting for some later close to (wrongly)
// complete it. A close event completes the pending open into a pair, if
// there is one; a close with nothing pending is an orphan and is left in
// place the same way.
func pairKindMatches(text string, kind SyntheticContentKind, spec *syntheticContentSpec) []syntheticMatch {
	occurrences := collectMarkerOccurrences(text, spec)
	var matches []syntheticMatch
	var pending *markerOccurrence
	for i := range occurrences {
		occ := &occurrences[i]
		if occ.isOpen {
			pending = occ
			continue
		}
		if pending == nil {
			continue
		}
		// An open marker's optional attribute values match any byte but a
		// double quote, so a value holding a literal close marker makes the
		// open match span that close. collectMarkerOccurrences orders on start
		// alone, so the spanning open is still pending when the swallowed close
		// arrives, and the body between them runs backwards. The pair carries
		// no body in that case, which is what the close being inside the open
		// already means. spliceSyntheticParts guards the same invariant for
		// overlapping matches.
		bodyStart, bodyEnd := pending.end, occ.start
		if bodyEnd < bodyStart {
			bodyEnd = bodyStart
		}
		matches = append(matches, syntheticMatch{
			kind:      kind,
			start:     pending.start,
			end:       consumeTrailingNewlines(text, occ.end),
			bodyTrim:  stripDecoration(spec, text[bodyStart:bodyEnd]),
			ref:       pending.ref,
			origin:    pending.origin,
			encrypted: occ.encrypted,
			signature: occ.signature,
		})
		pending = nil
	}
	return matches
}

// collectMarkerOccurrences finds every open and close marker occurrence for
// one kind and returns them merged into one text-order list. Both
// spec.openOnlyRE.FindAllStringSubmatchIndex and its closeOnlyRE
// counterpart already return their own matches in left-to-right order, so
// merging the two lists is a linear merge rather than a full sort.
func collectMarkerOccurrences(text string, spec *syntheticContentSpec) []markerOccurrence {
	openIdxs := spec.openOnlyRE.FindAllStringSubmatchIndex(text, -1)
	closeIdxs := spec.closeOnlyRE.FindAllStringSubmatchIndex(text, -1)

	opens := make([]markerOccurrence, len(openIdxs))
	for i, idx := range openIdxs {
		opens[i] = markerOccurrence{
			isOpen:    true,
			start:     idx[0],
			end:       idx[1],
			ref:       captureSubstring(text, idx[2], idx[3]),
			origin:    SyntheticOrigin(captureSubstring(text, idx[4], idx[5])),
			encrypted: "",
			signature: "",
		}
	}
	closes := make([]markerOccurrence, len(closeIdxs))
	for i, idx := range closeIdxs {
		closes[i] = markerOccurrence{
			isOpen:    false,
			start:     idx[0],
			end:       idx[1],
			ref:       "",
			origin:    OriginUnknown,
			encrypted: captureSubstring(text, idx[2], idx[3]),
			signature: captureSubstring(text, idx[4], idx[5]),
		}
	}

	merged := make([]markerOccurrence, 0, len(opens)+len(closes))
	i, j := 0, 0
	for i < len(opens) && j < len(closes) {
		if opens[i].start <= closes[j].start {
			merged = append(merged, opens[i])
			i++
		} else {
			merged = append(merged, closes[j])
			j++
		}
	}
	merged = append(merged, opens[i:]...)
	merged = append(merged, closes[j:]...)
	return merged
}

// consumeTrailingNewlines advances past from (the index right after a
// close tag's own "-->") over a run of literal newline characters,
// returning the first index that is not one. It is bounded to newline
// characters only, matching exactly the decorative blank line
// SyntheticContentCloseWithAttrs always appends after the close tag: a
// broader whitespace class would also consume real leading whitespace on
// whatever immediately follows (e.g. an indented code block line),
// destroying content that happens to sit right after a close marker with
// no other separator.
func consumeTrailingNewlines(text string, from int) int {
	end := from
	for end < len(text) && text[end] == '\n' {
		end++
	}
	return end
}

// captureSubstring returns text[start:end] when both indices are non-negative
// (regexp signals an absent optional group with -1). Empty otherwise.
func captureSubstring(text string, start, end int) string {
	if start < 0 || end < 0 {
		return ""
	}
	return text[start:end]
}

// sortSyntheticMatches puts matches in linear text order via an insertion
// sort. N is tiny (envelopes per assistant turn) so the simpler algorithm
// wins.
func sortSyntheticMatches(matches []syntheticMatch) {
	for i := 1; i < len(matches); i++ {
		j := i
		for j > 0 && matches[j-1].start > matches[j].start {
			matches[j-1], matches[j] = matches[j], matches[j-1]
			j--
		}
	}
}

// spliceSyntheticParts walks the ordered match list and produces the final
// part slice, interleaving text spans with the envelope parts.
func spliceSyntheticParts(text string, matches []syntheticMatch) []SyntheticPart {
	var parts []SyntheticPart
	cursor := 0
	for _, m := range matches {
		if m.start < cursor {
			continue
		}
		if m.start > cursor {
			parts = appendTextPart(parts, text[cursor:m.start])
		}
		parts = append(parts, SyntheticPart{Kind: m.kind, Body: m.bodyTrim, Ref: m.ref, Encrypted: m.encrypted, Signature: m.signature, Origin: m.origin})
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
	return append(parts, SyntheticPart{Kind: SyntheticKindText, Body: text, Ref: "", Encrypted: "", Signature: "", Origin: OriginUnknown})
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
	// MaterializedKindNativeRedactedThinking asks the mapper to emit an
	// upstream-native redacted_thinking content block carrying the
	// opaque encrypted blob in Body. Body is not human readable; only
	// Anthropic supports this in current code, and the mapper is
	// expected to forward it as `{"type":"redacted_thinking","data":Body}`.
	MaterializedKindNativeRedactedThinking MaterializedKind = "native_redacted_thinking"
)

// MaterializedPart is one ordered output instruction from
// [MaterializeSyntheticParts]. The provider mapper renders each part
// mechanically into its own upstream-native content block type.
//
// Signature carries the Anthropic per-thinking-block signature parsed off
// the close-marker `data-signature` attribute, propagated through the
// generic materializer so the Anthropic mapper can copy it onto the
// emitted native thinking content block. Empty for non-Anthropic parts
// and for text parts.
//
// Origin propagates [SyntheticPart.Origin] (the producing model family)
// so the provider mapper can decide between native replay (same family)
// and plain-text injection (foreign or unknown family) without re-parsing
// the source envelope.
type MaterializedPart struct {
	Kind      MaterializedKind
	Body      string
	Signature string
	Origin    SyntheticOrigin
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
			out = append(out, MaterializedPart{Kind: MaterializedKindText, Body: p.Body, Signature: "", Origin: OriginUnknown})
		case SyntheticReasoning:
			out = append(out, materializeReasoningPart(p.Body, p.Signature, p.Origin, strategy)...)
		case SyntheticRedactedThinking:
			out = append(out, materializeRedactedThinkingPart(p.Encrypted, p.Origin, strategy)...)
		case SyntheticNotice:
			continue
		}
	}
	return out
}

// materializeReasoningPart applies the configured strategy to a single
// reasoning part. The signature, when non-empty, is propagated only to
// MaterializedKindNativeThinking output so the Anthropic mapper can copy
// it onto the emitted thinking content block; other strategies drop the
// signature because the body is no longer rendered as a native thinking
// block.
func materializeReasoningPart(body, signature string, origin SyntheticOrigin, strategy MaterializationStrategy) []MaterializedPart {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil
	}
	switch strategy {
	case MaterializeNativeThinkingBlock:
		return []MaterializedPart{{Kind: MaterializedKindNativeThinking, Body: trimmed, Signature: signature, Origin: origin}}
	case MaterializePlainTextConcat:
		return []MaterializedPart{{Kind: MaterializedKindText, Body: trimmed, Signature: "", Origin: origin}}
	case MaterializePassthrough:
		envelope := FormatSyntheticContent(SyntheticReasoning, trimmed)
		if envelope == "" {
			return nil
		}
		return []MaterializedPart{{Kind: MaterializedKindText, Body: envelope, Signature: "", Origin: origin}}
	case MaterializeDrop:
		return nil
	}
	// Unknown strategy: behave like Drop so a future enum value never silently
	// leaks raw thinking content upstream.
	return nil
}

// materializeRedactedThinkingPart applies the configured strategy to a single
// redacted-thinking part. The opaque encrypted blob (parsed off the close
// marker's `data-encrypted` attribute) rides as the materialized Body so the
// Anthropic mapper can forward it as `{"type":"redacted_thinking","data":Body}`.
//
// MaterializeNativeThinkingBlock emits a single
// MaterializedKindNativeRedactedThinking output. MaterializeDrop discards the
// part. MaterializePlainTextConcat is intentionally a drop: the blob is not
// human readable, and folding it into prose would corrupt the assistant text
// while leaking opaque internal content into the visible context. Passthrough
// is also a drop because the redacted envelope is not a stable text shape an
// arbitrary upstream can replay.
func materializeRedactedThinkingPart(encrypted string, origin SyntheticOrigin, strategy MaterializationStrategy) []MaterializedPart {
	trimmed := strings.TrimSpace(encrypted)
	if trimmed == "" {
		return nil
	}
	if strategy == MaterializeNativeThinkingBlock {
		return []MaterializedPart{{Kind: MaterializedKindNativeRedactedThinking, Body: trimmed, Signature: "", Origin: origin}}
	}
	return nil
}
