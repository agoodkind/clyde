package antigravity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveClientVersionPrefersInstalledVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Info.plist")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	version, err := resolveClientVersion(
		path,
		"1.0.0",
		func(string) ([]byte, error) {
			return []byte("2.0.0\n"), nil
		},
	)
	if err != nil || version != "2.0.0" {
		t.Fatalf("version = %q, error = %v, want 2.0.0", version, err)
	}
}

func TestResolveClientVersionUsesConfigWhenAppIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Info.plist")
	readVersion := func(string) ([]byte, error) {
		t.Error("attempted to read an absent plist")

		return nil, os.ErrNotExist
	}

	version, err := resolveClientVersion(path, "1.0.0", readVersion)
	if err != nil || version != "1.0.0" {
		t.Fatalf("version = %q, error = %v, want 1.0.0", version, err)
	}

	if _, err := resolveClientVersion(path, "", readVersion); !errors.Is(
		err,
		errClientVersionRequired,
	) {
		t.Fatalf("error = %v, want missing client version", err)
	}
}
