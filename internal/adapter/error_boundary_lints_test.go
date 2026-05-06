package adapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errorBoundaryProhibitedSymbol enumerates the locked-down identifiers
// that must only be referenced from the boundary file set. The AST
// self-test below walks every adapter source file and fails if a
// non-allowlisted file mentions any of these. New locked symbols
// extend this enum; never widen the allowlist without updating the
// boundary contract first.
type errorBoundaryProhibitedSymbol string

const (
	prohibitedWriteJSONStatus errorBoundaryProhibitedSymbol = "WriteJSONStatus"
	prohibitedEmitStreamError errorBoundaryProhibitedSymbol = "EmitStreamError"
	prohibitedHTTPError       errorBoundaryProhibitedSymbol = "http.Error"
)

func errorBoundaryProhibitedSymbols() []errorBoundaryProhibitedSymbol {
	return []errorBoundaryProhibitedSymbol{
		prohibitedWriteJSONStatus,
		prohibitedEmitStreamError,
		prohibitedHTTPError,
	}
}

// errorBoundaryAllowlist enumerates the production files that are
// permitted to reference the locked-down symbols. _test.go files that
// legitimately exercise error-handling helpers are also allowed; the
// allowlist below is the production-side gate.
//
// Provider envelope renderers
// (internal/adapter/openai/error_envelope.go and
// internal/adapter/anthropic/error_envelope.go) live in subpackages
// and are not walked by this self-test; the AST scan only inspects
// the adapter package's own *.go files. Adding a new file that
// renders an error envelope MUST therefore live in a provider
// package and call WriteJSONStatus from there, not from this
// package.
func errorBoundaryAllowlist() map[string]bool {
	return map[string]bool{
		"adapter_error.go":                true,
		"family_error_mapping.go":         true,
		"error_boundary_lints_test.go":    true,
		"error_boundary_contract_test.go": true,
	}
}

// TestErrorBoundaryNoUnshapedErrorWrites walks the adapter package's
// own AST and asserts that no non-allowlisted file references the
// prohibited symbols. The helper functions for stream errors and the
// shaped writer live behind boundary-internal helpers; bypassing them
// is what regresses Cursor BYOK rendering.
func TestErrorBoundaryNoUnshapedErrorWrites(t *testing.T) {
	t.Parallel()
	allowlist := errorBoundaryAllowlist()
	prohibited := errorBoundaryProhibitedSymbols()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read adapter directory: %v", err)
	}
	fset := token.NewFileSet()
	violations := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if allowlist[name] {
			continue
		}
		// Test files that legitimately test error handling are
		// allowed; the non-test file gate is the production lockdown.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(".", name)
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			candidates := identifierCandidatesFromNode(node)
			if len(candidates) == 0 {
				return true
			}
			for _, candidate := range candidates {
				for _, sym := range prohibited {
					if candidate == string(sym) {
						pos := fset.Position(node.Pos())
						t.Errorf("%s:%d: prohibited symbol %q referenced outside boundary allowlist", pos.Filename, pos.Line, sym)
						violations++
					}
				}
			}
			return true
		})
	}
	if violations > 0 {
		t.Fatalf("error boundary self-test found %d violations", violations)
	}
}

func TestTopLevelAdapterProductionDoesNotOwnOpenAIErrorBodies(t *testing.T) {
	t.Parallel()
	prohibited := map[string]bool{
		"ErrorBody":                   true,
		"ErrorResponse":               true,
		"adapteropenai.ErrorBody":     true,
		"adapteropenai.ErrorResponse": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read adapter directory: %v", err)
	}
	fset := token.NewFileSet()
	violations := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(".", name)
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			for _, candidate := range identifierCandidatesFromNode(node) {
				if !prohibited[candidate] {
					continue
				}
				pos := fset.Position(node.Pos())
				t.Errorf("%s:%d: OpenAI error envelope symbol %q must stay in provider packages", pos.Filename, pos.Line, candidate)
				violations++
			}
			return true
		})
	}
	if violations > 0 {
		t.Fatalf("OpenAI error body ownership self-test found %d violations", violations)
	}
}

// identifierCandidatesFromNode returns every textual form by which a
// node could reference a prohibited symbol. SelectorExpr nodes
// produce both the qualified form (http.Error) and the bare method
// name (Error or EmitStreamError) so a method call through a typed
// receiver still matches a method-name lockdown entry.
func identifierCandidatesFromNode(node ast.Node) []string {
	switch typed := node.(type) {
	case *ast.Ident:
		return []string{typed.Name}
	case *ast.SelectorExpr:
		out := []string{typed.Sel.Name}
		if x, ok := typed.X.(*ast.Ident); ok {
			out = append(out, x.Name+"."+typed.Sel.Name)
		}
		return out
	}
	return nil
}
