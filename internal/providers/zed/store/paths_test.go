package zedstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDataRootsUsesDefaultMacLocation(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	roots, err := ResolveDataRoots(t.Context(), "")
	if err != nil {
		t.Fatalf("ResolveDataRoots returned error: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("roots len = %d, want 1", len(roots))
	}

	expectedRoot := filepath.Join(homeDir, "Library", "Application Support", "Zed")
	root := roots[0]
	if root.RootDir != expectedRoot {
		t.Fatalf("RootDir = %q, want %q", root.RootDir, expectedRoot)
	}
	if root.ThreadsDBPath != filepath.Join(expectedRoot, "threads", "threads.db") {
		t.Fatalf("ThreadsDBPath = %q", root.ThreadsDBPath)
	}
	if len(root.MetadataDBPaths) != 4 {
		t.Fatalf("MetadataDBPaths len = %d, want 4", len(root.MetadataDBPaths))
	}
	if root.MetadataDBPaths[0] != filepath.Join(expectedRoot, "db", "0-stable", "db.sqlite") {
		t.Fatalf("stable metadata db path = %q", root.MetadataDBPaths[0])
	}
}

func TestResolveDataRootsUsesPlatformPathListOverride(t *testing.T) {
	rootA := filepath.Join(t.TempDir(), "zed-a")
	rootB := filepath.Join(t.TempDir(), "zed-b")
	override := " " + rootA + " " + string(os.PathListSeparator) + " " + rootB + string(os.PathListSeparator) + rootA + " "

	roots, err := ResolveDataRoots(t.Context(), override)
	if err != nil {
		t.Fatalf("ResolveDataRoots returned error: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("roots len = %d, want 2", len(roots))
	}
	if roots[0].RootDir != rootA {
		t.Fatalf("roots[0].RootDir = %q, want %q", roots[0].RootDir, rootA)
	}
	if roots[1].RootDir != rootB {
		t.Fatalf("roots[1].RootDir = %q, want %q", roots[1].RootDir, rootB)
	}
}
