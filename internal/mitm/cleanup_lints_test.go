package mitm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMITMCleanupSymbolsRemainAbsent fails if any banned launcher-era symbol
// reappears anywhere under cmd/ or internal/. The cleanup that landed in
// CLYDE-265 deleted the Electron-launcher cruft and the daemon RPCs that
// powered it; this guard makes sure a future change cannot quietly bring
// any of those names back. The walk includes test files so test fixtures
// cannot be the reintroduction vector either.
func TestMITMCleanupSymbolsRemainAbsent(t *testing.T) {
	repoRoot, err := mitmCleanupRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	bannedIdents := map[string]string{
		"LaunchMITMUpstream":    "Electron launcher RPC",
		"PrepareMITMLaunch":     "Electron launcher RPC",
		"LaunchProfile":         "Electron launcher type",
		"LaunchProfileOptions":  "Electron launcher options type",
		"configureCursorLaunch": "Cursor launch configurator",
	}
	bannedFlagLiterals := map[string]string{
		"--exec-binary": "exec-binary CLI flag",
	}
	roots := []string{
		filepath.Join(repoRoot, "cmd"),
		filepath.Join(repoRoot, "internal"),
	}
	for _, root := range roots {
		assertNoBannedSymbols(t, root, bannedIdents, bannedFlagLiterals)
	}
}

func assertNoBannedSymbols(t *testing.T, root string, bannedIdents, bannedFlagLiterals map[string]string) {
	t.Helper()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	}
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip the guard itself; the banned literals appear in this file's
		// banned-name maps and would otherwise trip on themselves.
		if strings.HasSuffix(path, "cleanup_lints_test.go") {
			return nil
		}
		// Skip generated protobuf code; the cleanup removed the RPCs upstream
		// at proto level and the regenerated stubs are clean, so any future
		// reintroduction must come through the .proto, not regenerated stubs.
		if strings.HasSuffix(path, ".pb.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.Ident:
				if reason, banned := bannedIdents[typed.Name]; banned {
					t.Errorf("%s:%d: identifier %q reappeared (%s)", path, fset.Position(typed.Pos()).Line, typed.Name, reason)
				}
			case *ast.BasicLit:
				if typed.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(typed.Value)
				if err != nil {
					return true
				}
				if reason, banned := bannedFlagLiterals[value]; banned {
					t.Errorf("%s:%d: string literal %q reappeared (%s)", path, fset.Position(typed.Pos()).Line, value, reason)
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
}

// mitmCleanupRepoRoot walks up from this file's location until it finds a
// go.mod, since `go test` runs each package in its own working directory.
func mitmCleanupRepoRoot() (string, error) {
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
