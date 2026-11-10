package codex

import (
	_ "embed"
	"regexp"
	"strings"
	"testing"
)

// vendoredApplyPatchGrammar is the codex apply_patch LARK grammar copied
// from research/codex/codex-rs/core/src/tools/handlers/apply_patch.lark.
// The parity test reads the structural tokens straight out of this file
// so the assertions stay tied to the vendored grammar rather than to
// duplicated string literals.
//
//go:embed apply_patch.lark
var vendoredApplyPatchGrammar string

// grammarLiteralRe extracts the quoted string literals from a single
// LARK rule line (e.g. "*** Begin Patch" from `begin_patch: "*** Begin
// Patch" LF`). LARK string terminals are double-quoted.
var grammarLiteralRe = regexp.MustCompile(`"([^"]*)"`)

// grammarRuleLiterals returns the quoted literals on the named rule's
// line in the vendored grammar. It fails the test if the rule is
// missing, which guards against silent grammar drift on a codex sync.
func grammarRuleLiterals(t *testing.T, rule string) []string {
	t.Helper()
	for _, line := range strings.Split(vendoredApplyPatchGrammar, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		name, body, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(name) != rule {
			continue
		}
		matches := grammarLiteralRe.FindAllStringSubmatch(body, -1)
		out := make([]string, 0, len(matches))
		for _, m := range matches {
			out = append(out, m[1])
		}
		return out
	}
	t.Fatalf("rule %q not found in vendored apply_patch.lark", rule)
	return nil
}

// firstLiteral returns the first quoted literal on a grammar rule line.
func firstLiteral(t *testing.T, rule string) string {
	t.Helper()
	literals := grammarRuleLiterals(t, rule)
	if len(literals) == 0 {
		t.Fatalf("rule %q has no string literal", rule)
	}
	return literals[0]
}

// TestRepairApplyPatchOutputMatchesVendoredGrammarStructure asserts that
// the repaired output of the existing repair test case is consistent
// with the vendored apply_patch LARK grammar: it opens with the
// begin_patch terminal, closes with the end_patch terminal, every hunk
// header is one of the grammar's add/delete/update hunk terminals, and
// no orphaned update header sits directly in front of another hunk
// header (the malformed shape the grammar's `hunk` rule rejects). This
// is a structural check, not a full LARK parse, but it is keyed off the
// terminals the vendored grammar defines.
func TestRepairApplyPatchOutputMatchesVendoredGrammarStructure(t *testing.T) {
	// Same input as TestApplyPatchArgsRepairsNestedAddFileHeaderInsideUpdateWrapper.
	input := "*** Begin Patch\n" +
		"*** Update File: /tmp/render_scope_test.go\n" +
		"*** Add File: /tmp/render_scope_test.go\n" +
		"+package tools\n" +
		"*** End Patch\n"

	repaired, ok := ApplyPatchArgs(input)
	if !ok {
		t.Fatal("ApplyPatchArgs returned !ok")
	}

	beginPatch := firstLiteral(t, "begin_patch")
	endPatch := firstLiteral(t, "end_patch")
	addHunk := firstLiteral(t, "add_hunk")
	deleteHunk := firstLiteral(t, "delete_hunk")
	updateHunk := firstLiteral(t, "update_hunk")
	hunkHeaders := []string{addHunk, deleteHunk, updateHunk}

	lines := strings.Split(strings.TrimRight(repaired, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("repaired patch too short: %q", repaired)
	}
	if strings.TrimSpace(lines[0]) != beginPatch {
		t.Fatalf("repaired patch first line=%q want begin_patch %q", lines[0], beginPatch)
	}
	if strings.TrimSpace(lines[len(lines)-1]) != endPatch {
		t.Fatalf("repaired patch last line=%q want end_patch %q", lines[len(lines)-1], endPatch)
	}

	// Collect the hunk-header line indices using the grammar terminals.
	var headerIndices []int
	for i, line := range lines {
		for _, header := range hunkHeaders {
			if strings.HasPrefix(line, header) {
				headerIndices = append(headerIndices, i)
				break
			}
		}
	}
	if len(headerIndices) == 0 {
		t.Fatalf("repaired patch has no grammar hunk header: %q", repaired)
	}

	// The grammar's `hunk: add_hunk | delete_hunk | update_hunk` rule
	// makes a bare update header that is immediately followed by another
	// hunk header malformed (an update_hunk needs a change body). The
	// repair must have dropped the orphaned update header from the input.
	for _, idx := range headerIndices {
		if !strings.HasPrefix(lines[idx], updateHunk) {
			continue
		}
		if idx+1 >= len(lines) {
			continue
		}
		next := lines[idx+1]
		for _, header := range hunkHeaders {
			if strings.HasPrefix(next, header) {
				t.Fatalf("repaired patch keeps orphaned update header before another hunk: %q then %q", lines[idx], next)
			}
		}
	}

	// The surviving hunk must be the well-formed add_hunk with its add_line.
	if !strings.HasPrefix(lines[headerIndices[0]], addHunk) {
		t.Fatalf("repaired patch first hunk=%q want add_hunk %q", lines[headerIndices[0]], addHunk)
	}
	addLinePrefix := firstLiteral(t, "add_line")
	if !strings.HasPrefix(lines[headerIndices[0]+1], addLinePrefix) {
		t.Fatalf("repaired add_hunk body=%q want add_line prefix %q", lines[headerIndices[0]+1], addLinePrefix)
	}
}
