package parser

import "testing"

func TestBuildParseVirtualPathRoundTrip(t *testing.T) {
	t.Parallel()

	rootHash := RootHash("/Users/alice/Library/Application Support/Cursor/User")
	path := BuildVirtualPath(rootHash, VirtualKindComposer, "composer-a")
	if path == "" {
		t.Fatal("BuildVirtualPath returned empty path")
	}

	parsed, err := ParseVirtualPath(path)
	if err != nil {
		t.Fatalf("ParseVirtualPath returned error: %v", err)
	}
	if parsed.RootHash != rootHash {
		t.Fatalf("RootHash = %q, want %q", parsed.RootHash, rootHash)
	}
	if parsed.Kind != VirtualKindComposer {
		t.Fatalf("Kind = %q, want %q", parsed.Kind, VirtualKindComposer)
	}
	if parsed.ID != "composer-a" {
		t.Fatalf("ID = %q, want composer-a", parsed.ID)
	}
}

func TestLegacyIDRoundTrip(t *testing.T) {
	t.Parallel()

	id := legacyID("workspace-hash", "tab-a")
	workspaceHash, tabID, ok := splitLegacyID(id)
	if !ok {
		t.Fatal("splitLegacyID ok = false, want true")
	}
	if workspaceHash != "workspace-hash" {
		t.Fatalf("workspaceHash = %q, want workspace-hash", workspaceHash)
	}
	if tabID != "tab-a" {
		t.Fatalf("tabID = %q, want tab-a", tabID)
	}
}

func TestBuildVirtualPathRejectsUnsafeParts(t *testing.T) {
	t.Parallel()

	rootHash := RootHash("/tmp/cursor")
	cases := []struct {
		name     string
		rootHash string
		kind     string
		id       string
	}{
		{name: "empty root", rootHash: "", kind: VirtualKindComposer, id: "composer-a"},
		{name: "slash kind", rootHash: rootHash, kind: "bad/kind", id: "composer-a"},
		{name: "query id", rootHash: rootHash, kind: VirtualKindComposer, id: "composer?bad"},
		{name: "fragment id", rootHash: rootHash, kind: VirtualKindComposer, id: "composer#bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := BuildVirtualPath(tc.rootHash, tc.kind, tc.id)
			if path != "" {
				t.Fatalf("BuildVirtualPath returned %q, want empty", path)
			}
		})
	}
}

func TestParseVirtualPathRejectsMalformedPaths(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"zed://0123456789abcdef/composer/id",
		"cursor://0123456789abcde/composer/id",
		"cursor://not-hex-not-hex/composer/id",
		"cursor://0123456789abcdef/composer",
		"cursor://0123456789abcdef/composer/id/extra",
		"cursor://0123456789abcdef//id",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			_, err := ParseVirtualPath(raw)
			if err == nil {
				t.Fatalf("ParseVirtualPath(%q) returned nil error, want error", raw)
			}
		})
	}
}

func TestSplitLegacyIDRejectsMalformedIDs(t *testing.T) {
	t.Parallel()

	cases := []string{"", "workspace", "workspace~", "~tab", "a~b~c"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			_, _, ok := splitLegacyID(raw)
			if ok {
				t.Fatalf("splitLegacyID(%q) ok = true, want false", raw)
			}
		})
	}
}
