package truststore

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenStatusImports lists the packages a read-only status path
// must never reach for. Any of these would let the file mutate the
// trust store, the filesystem, or the environment, and that is the
// exact thing the architectural rule for status paths forbids.
var forbiddenStatusImports = []string{
	"os/exec",
	"syscall",
	"goodkind.io/clyde/internal/cli/mitm/truststore/exec",
}

// TestStatusFilesDoNotImportMutatingHelpers walks every file in the
// package whose name matches truststore_status*.go and parses its
// import set. The test fails when any matched file imports a package
// from forbiddenStatusImports so the read-only rule is enforced
// without depending on any one platform's build tag.
func TestStatusFilesDoNotImportMutatingHelpers(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	matched := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "truststore_status") {
			continue
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		matched++
		assertFileHasNoForbiddenImport(t, name)
	}
	if matched == 0 {
		t.Fatalf("no truststore_status*.go files matched; the lint cannot run")
	}
}

func assertFileHasNoForbiddenImport(t *testing.T, name string) {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	for _, importSpec := range parsed.Imports {
		path := strings.Trim(importSpec.Path.Value, `"`)
		for _, banned := range forbiddenStatusImports {
			if path == banned {
				t.Fatalf("file %s imports forbidden package %q; status path must stay read-only", name, banned)
			}
		}
	}
}
