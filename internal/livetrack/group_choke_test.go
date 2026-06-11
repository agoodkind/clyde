package livetrack

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoExportedConstructorOrDrain asserts the package no longer exports New,
// Drain, or DrainWith. The only way to obtain a registry is Attach, and the only
// way to drain is Group.Quiesce. This is the structural choke point: a registry
// outside a group cannot be constructed, enforced at the API surface rather than
// by a launderable runtime convention.
func TestNoExportedConstructorOrDrain(t *testing.T) {
	t.Parallel()
	forbidden := map[string]bool{"New": true, "Drain": true, "DrainWith": true}
	fset := token.NewFileSet()
	for _, file := range parseLivetrackPackageFiles(t, fset) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if forbidden[fn.Name.Name] && fn.Name.IsExported() {
				t.Errorf("forbidden exported lifecycle entry point still present: %s", fn.Name.Name)
			}
		}
	}
}

// TestRegistryConstructionOnlyViaAttach asserts newRegistry is called only
// inside Attach, so the group-membership invariant cannot be bypassed even
// within the package. With New unexported and this guard in place, every
// registry is a declared group member.
func TestRegistryConstructionOnlyViaAttach(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, file := range parseLivetrackPackageFiles(t, fset) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if callsNewRegistry(fn.Body) && fn.Name.Name != "Attach" {
				pos := fset.Position(fn.Pos())
				t.Errorf("%s:%d: newRegistry called outside Attach in %q; registries must be constructed through Attach",
					pos.Filename, pos.Line, fn.Name.Name)
			}
		}
	}
}

// callsNewRegistry reports whether body contains a call to newRegistry.
func callsNewRegistry(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "newRegistry" {
				found = true
			}
		case *ast.IndexExpr:
			if ident, ok := fn.X.(*ast.Ident); ok && ident.Name == "newRegistry" {
				found = true
			}
		}
		return !found
	})
	return found
}

// parseLivetrackPackageFiles parses every non-test .go file in the livetrack
// package directory.
func parseLivetrackPackageFiles(t *testing.T, fset *token.FileSet) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	files := make([]*ast.File, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		files = append(files, parsed)
	}
	return files
}
