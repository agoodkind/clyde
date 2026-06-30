package codex

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeInstallationFinder struct {
	codex string
	clyde string
}

func (f fakeInstallationFinder) codexPath() (string, error) { return f.codex, nil }
func (f fakeInstallationFinder) clydePath() (string, error) { return f.clyde, nil }

func TestInstallationFileFinderClydePathUsesConfigResolver(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "~/xdg/../config-root")

	got, err := installationFileFinder{}.clydePath()
	if err != nil {
		t.Fatalf("clydePath: %v", err)
	}
	want := filepath.Join(home, "config-root", "clyde", "codex-installation-id")
	if got != want {
		t.Fatalf("clydePath=%q want %q", got, want)
	}
}

func TestLoadInstallationIDReadsCodexFileWhenPresent(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "installation_id")
	if err := os.WriteFile(codex, []byte("real-codex-id-1234\n"), 0o600); err != nil {
		t.Fatalf("write codex file: %v", err)
	}
	loader := newInstallationLoader(fakeInstallationFinder{codex: codex, clyde: filepath.Join(dir, "unused")})
	got, err := loader.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "real-codex-id-1234" {
		t.Errorf("expected codex id, got %q", got)
	}
}

func TestLoadInstallationIDReadsClydeFileWhenCodexMissing(t *testing.T) {
	dir := t.TempDir()
	clyde := filepath.Join(dir, "clyde-id")
	if err := os.WriteFile(clyde, []byte("persisted-clyde-id-9999\n"), 0o600); err != nil {
		t.Fatalf("write clyde file: %v", err)
	}
	loader := newInstallationLoader(fakeInstallationFinder{codex: filepath.Join(dir, "missing"), clyde: clyde})
	got, err := loader.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "persisted-clyde-id-9999" {
		t.Errorf("expected clyde id, got %q", got)
	}
}

func TestLoadInstallationIDGeneratesAndPersistsWhenBothMissing(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "no-codex")
	clyde := filepath.Join(dir, "subdir", "clyde-id")
	loader := newInstallationLoader(fakeInstallationFinder{codex: codex, clyde: clyde})
	first, err := loader.Load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(first) != 32 {
		t.Errorf("expected 32-char hex id, got %q", first)
	}
	raw, err := os.ReadFile(clyde)
	if err != nil {
		t.Fatalf("read persisted: %v", err)
	}
	persisted := string(raw)
	if persisted == "" || persisted[:len(persisted)-1] != first {
		t.Errorf("persisted file %q does not contain id %q", persisted, first)
	}
	loader2 := newInstallationLoader(fakeInstallationFinder{codex: codex, clyde: clyde})
	second, err := loader2.Load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second != first {
		t.Errorf("expected stable id across loads, got %q vs %q", first, second)
	}
}

func TestLoadInstallationIDCachesAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "installation_id")
	if err := os.WriteFile(codex, []byte("first-value\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	loader := newInstallationLoader(fakeInstallationFinder{codex: codex, clyde: filepath.Join(dir, "unused")})
	first, err := loader.Load()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := os.WriteFile(codex, []byte("second-value\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	second, err := loader.Load()
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Errorf("cache violated: %q != %q", first, second)
	}
}
