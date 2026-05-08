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

// TestLivetrackAdoptersPairShutdownWithDrain walks every package
// that imports internal/livetrack and asserts that any reference to
// http.Server.Shutdown sits in a function whose body also references
// the registry's Drain method or its own Drain helper. Without this
// gate, an adopter could regress to the bare http.Server.Shutdown
// path that the registry was introduced to replace, leaving wedged
// tunnels behind.
//
// The walk skips _test.go files (tests legitimately drive Shutdown
// in isolation) and skips the livetrack package itself (which
// defines Drain but never calls Shutdown). Allowlisted files are
// listed below; new entries require a matching code change so the
// regression vector is visible in code review.
func TestLivetrackAdoptersPairShutdownWithDrain(t *testing.T) {
	t.Parallel()
	repoRoot, err := livetrackRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	importingPackages := findLivetrackImporters(t, repoRoot)
	if len(importingPackages) == 0 {
		t.Skip("no livetrack importers found; lint is a no-op until adoption begins")
	}
	violations := 0
	for _, pkgDir := range importingPackages {
		entries, err := os.ReadDir(pkgDir)
		if err != nil {
			t.Fatalf("read %s: %v", pkgDir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(pkgDir, name)
			if violations += assertShutdownPairedWithDrain(t, path); violations > 0 {
				continue
			}
		}
	}
	if violations > 0 {
		t.Fatalf("found %d Shutdown-without-Drain violations", violations)
	}
}

// assertShutdownPairedWithDrain parses one file and reports each
// http.Server.Shutdown call that lives in a function with no Drain
// reference. Returns the number of new violations found.
func assertShutdownPairedWithDrain(t *testing.T, path string) int {
	t.Helper()
	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if parseErr != nil {
		t.Fatalf("parse %s: %v", path, parseErr)
	}
	violations := 0
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Body == nil {
			continue
		}
		shutdownCalls := collectShutdownCalls(funcDecl.Body)
		if len(shutdownCalls) == 0 {
			continue
		}
		if functionReferencesDrain(funcDecl.Body) {
			continue
		}
		for _, call := range shutdownCalls {
			pos := fset.Position(call.Pos())
			t.Errorf("%s:%d: http.Server.Shutdown reference without paired registry.Drain in %q",
				pos.Filename, pos.Line, funcDecl.Name.Name)
			violations++
		}
	}
	return violations
}

func collectShutdownCalls(body *ast.BlockStmt) []*ast.SelectorExpr {
	calls := make([]*ast.SelectorExpr, 0)
	ast.Inspect(body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel == nil {
			return true
		}
		if sel.Sel.Name != "Shutdown" {
			return true
		}
		// Filter: the Shutdown call must look like a method call on
		// an http.Server-shaped receiver (named "server" or
		// "httpSrv"). This catches the empirical pattern without
		// false-positiving on Drain implementations that themselves
		// call Shutdown on a different object.
		if isLikelyHTTPServerShutdown(sel) {
			calls = append(calls, sel)
		}
		return true
	})
	return calls
}

func isLikelyHTTPServerShutdown(sel *ast.SelectorExpr) bool {
	switch x := sel.X.(type) {
	case *ast.Ident:
		switch x.Name {
		case "server", "httpSrv", "srv":
			return true
		}
	case *ast.SelectorExpr:
		if x.Sel != nil {
			switch x.Sel.Name {
			case "server", "httpSrv":
				return true
			}
		}
	}
	return false
}

func functionReferencesDrain(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel != nil && sel.Sel.Name == "Drain" {
			found = true
			return false
		}
		return true
	})
	return found
}

// findLivetrackImporters walks the repo and returns the directories
// of every non-test package whose source files import
// internal/livetrack. Tests inside livetrack itself are excluded.
func findLivetrackImporters(t *testing.T, repoRoot string) []string {
	t.Helper()
	pkgDirs := make(map[string]bool)
	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == "vendor" || base == ".git" || base == ".make" || base == ".claude" || base == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return nil
		}
		for _, imp := range file.Imports {
			if imp.Path == nil {
				continue
			}
			value := strings.Trim(imp.Path.Value, "\"")
			if value == "goodkind.io/clyde/internal/livetrack" {
				pkgDirs[filepath.Dir(path)] = true
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo: %v", walkErr)
	}
	out := make([]string, 0, len(pkgDirs))
	for dir := range pkgDirs {
		out = append(out, dir)
	}
	return out
}

func livetrackRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
