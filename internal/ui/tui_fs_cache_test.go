package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// TestShortPathPure checks the current shortPath helper behavior against a
// stable HOME setting.
func TestShortPathPure(t *testing.T) {
	t.Setenv("HOME", "/Users/test")
	cases := []struct {
		name string
		root string
		want string
	}{
		{name: "empty root returns dash", root: "", want: "-"},
		{name: "root equals home collapses to tilde", root: "/Users/test", want: "~"},
		{name: "root under home gets relative tilde prefix", root: "/Users/test/Sites/repo", want: "~/Sites/repo"},
		{name: "root outside home is returned unchanged", root: "/var/log/clyde", want: "/var/log/clyde"},
		{name: "root shorter than home stays absolute", root: "/Users", want: "/Users"},
		{name: "home prefix without trailing slash does not match", root: "/Users/testing/Sites/repo", want: "/Users/testing/Sites/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shortPath(tc.root)
			if got != tc.want {
				t.Fatalf("shortPath(%q) = %q, want %q", tc.root, got, tc.want)
			}
		})
	}
}

func TestSettingsTabDrawDoesNotCallFilesystem(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	appFile := filepath.Join(filepath.Dir(currentFile), "app.go")
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, appFile, nil, 0)
	if err != nil {
		t.Fatalf("ParseFile(%q): %v", appFile, err)
	}

	var drawSettingsTab *ast.FuncDecl
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "drawSettingsTab" {
			continue
		}
		drawSettingsTab = fn
		break
	}
	if drawSettingsTab == nil {
		t.Fatal("drawSettingsTab not found")
	}

	forbidden := map[string]bool{
		"os.Getwd":       true,
		"os.Stat":        true,
		"os.UserHomeDir": true,
	}
	ast.Inspect(drawSettingsTab.Body, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		call := ident.Name + "." + selector.Sel.Name
		if forbidden[call] {
			position := fileSet.Position(selector.Pos())
			t.Fatalf("drawSettingsTab calls %s at %s", call, position)
		}
		return true
	})
}
