package conversation_test

import (
	"testing"

	"goodkind.io/clyde/internal/conversation"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	zedparser "goodkind.io/clyde/internal/providers/zed/parser"
)

// TestZedParserDoesNotImplementTailParser guards the design decision behind
// LoadRecentTurns's fallback: Zed's Stream already materializes the whole
// thread document from a SQLite blob during Discover before yielding
// anything, so there is no bounded byte range to read, and Zed deliberately
// does not implement conversation.TailParser. If this test starts failing,
// Zed gained a byte-addressable artifact format and this reasoning (and the
// tail-read report) needs to be revisited, not silently invalidated.
func TestZedParserDoesNotImplementTailParser(t *testing.T) {
	t.Parallel()
	var parser conversation.Parser = zedparser.New()
	if _, ok := parser.(conversation.TailParser); ok {
		t.Fatalf("zed parser now implements conversation.TailParser; LoadRecentTurns would start reading it as byte-addressable JSONL, which its SQLite-backed Stream does not support")
	}
}

// TestCursorParserDoesNotImplementTailParser guards the same decision for
// Cursor. One Cursor *Parser serves three artifact kinds (a byte-addressable
// JSONL kind, plus SQLite-backed composer and legacy kinds); a provider-level
// type assertion cannot distinguish between them, and this scope
// deliberately leaves Cursor out of TailParser entirely rather than
// implementing TailSize/StreamFrom for only the JSONL kind. Every Cursor
// record therefore falls back to the full load unchanged, for all three
// kinds, until a follow-up adds the JSONL kind's bounded read.
func TestCursorParserDoesNotImplementTailParser(t *testing.T) {
	t.Parallel()
	var parser conversation.Parser = cursorparser.New()
	if _, ok := parser.(conversation.TailParser); ok {
		t.Fatalf("cursor parser now implements conversation.TailParser; since one Parser serves JSONL, composer, and legacy kinds, TailSize must gate per artifact path, not per provider, or composer/legacy records would be misrouted into StreamFrom")
	}
}
