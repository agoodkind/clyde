// Package render owns the single source of truth for synthetic content
// envelopes that Clyde injects into Cursor's BYOK chat surface.
//
// Cursor's custom-OpenAI ingress reliably renders delta.content but does not
// honor secondary fields like reasoning_content for BYOK streams. To get the
// same visible affordances as Cursor's first-party providers (collapsible
// reasoning blocks, transient quota notices) we wrap synthetic UI bodies in
// HTML-comment marker pairs and emit them as ordinary delta.content. Every
// surface that emits synthetic content uses [FormatSyntheticContent], and
// every backend that needs to scrub these envelopes before reusing the
// transcript upstream uses [StripSyntheticContent]. Adding a new synthetic
// block is a single entry in [syntheticContentSpecs].
package render

import (
	"regexp"
	"strings"
)

// SyntheticContentKind identifies a marker-wrapped synthetic block kind.
type SyntheticContentKind string

const (
	// SyntheticReasoning wraps Cursor-visible reasoning bodies emitted to
	// delta.content alongside delta.reasoning_content.
	SyntheticReasoning SyntheticContentKind = "thinking"
	// SyntheticNotice wraps transient quota and runtime notices emitted to
	// delta.content so they render as warning blockquotes in Cursor BYOK.
	SyntheticNotice SyntheticContentKind = "notice"
)

// syntheticContentSpec describes the rendering and stripping rules for one
// synthetic content kind. Header is the visible block header rendered inside
// the open marker; QuotePrefix means each body line is rendered as a markdown
// blockquote line.
type syntheticContentSpec struct {
	Marker      string
	Header      string
	QuotePrefix bool
	stripRE     *regexp.Regexp
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

func init() {
	for _, spec := range syntheticContentSpecs {
		spec.stripRE = regexp.MustCompile(`(?s)<!--` + regexp.QuoteMeta(spec.Marker) + `-->.*?<!--/` + regexp.QuoteMeta(spec.Marker) + `-->\s*`)
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

// StripSyntheticContent removes every recognized synthetic envelope from text
// in one pass so backend mappers can call it once before reusing assistant
// content for an upstream request. It is idempotent and falls through cleanly
// when no markers are present.
func StripSyntheticContent(text string) string {
	if text == "" {
		return ""
	}
	for _, spec := range syntheticContentSpecs {
		if !strings.Contains(text, "<!--"+spec.Marker+"-->") {
			continue
		}
		text = spec.stripRE.ReplaceAllString(text, "")
	}
	return text
}
