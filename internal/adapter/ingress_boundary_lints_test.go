package adapter

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ingressBoundaryAllowlist enumerates the production files that are
// permitted to import the cursor ingress package
// (internal/adapter/cursor). The composition root for vendor ingress
// registration is intentionally the only entry. New depth-1 adapter
// files MUST stay vendor-blind: route vendor knowledge through the
// IngressContract registered in family_ingress_mapping.go.
//
// Test files (_test.go) are not walked by this lint; the
// production-side gate is the boundary.
func ingressBoundaryAllowlist() map[string]bool {
	return map[string]bool{
		"ingress_registration.go": true,
	}
}

// cursorIngressImportPath is the canonical import path the lint
// detects. The adapter ingress boundary forbids non-allowlisted
// production files from importing this path; vendors plug in via the
// composition root only.
const cursorIngressImportPath = "goodkind.io/clyde/internal/adapter/cursor"

// TestIngressBoundaryNoCursorImportOutsideRegistration walks the
// adapter package's own production files and asserts that no
// non-allowlisted file imports the cursor ingress package. Test
// files are skipped because the production lockdown is what this lint
// guards; tests may use the cursor package to drive integration
// scenarios.
//
// Mirrors TestErrorBoundaryNoUnshapedErrorWrites in spirit: the
// generic adapter never speaks vendor ingress shape outside one
// composition-root file, and an AST self-test makes that rule
// reviewable.
func TestIngressBoundaryNoCursorImportOutsideRegistration(t *testing.T) {
	t.Parallel()
	allowlist := ingressBoundaryAllowlist()

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
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if allowlist[name] {
			continue
		}
		path := filepath.Join(".", name)
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		for _, imp := range file.Imports {
			pathLiteral := strings.Trim(imp.Path.Value, `"`)
			if pathLiteral != cursorIngressImportPath {
				continue
			}
			pos := fset.Position(imp.Pos())
			t.Errorf("%s:%d: cursor ingress package %q imported outside ingress boundary allowlist", pos.Filename, pos.Line, pathLiteral)
			violations++
		}
	}
	if violations > 0 {
		t.Fatalf("ingress boundary self-test found %d violations", violations)
	}
}
