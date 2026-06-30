package parser

import "testing"

func TestBuildAndParseVirtualPathRoundTrip(t *testing.T) {
	t.Parallel()

	path := buildVirtualPathFromRootHash(RootHash("/Users/me/Library/Application Support/Zed"), "0-stable", "thread-123")
	if path == "" {
		t.Fatal("buildVirtualPathFromRootHash returned empty path")
	}

	parsed, err := ParseVirtualPath(path)
	if err != nil {
		t.Fatalf("ParseVirtualPath returned error: %v", err)
	}
	if parsed.Channel != "0-stable" || parsed.SessionID != "thread-123" {
		t.Fatalf("parsed = %#v", parsed)
	}

	second := buildVirtualPathFromRootHash(RootHash("/Users/me/Library/Application Support/Zed"), "0-stable", "thread-123")
	if second != path {
		t.Fatalf("buildVirtualPathFromRootHash was not stable: %q != %q", second, path)
	}
}

func TestRootHashRejectsEmptyRootDir(t *testing.T) {
	t.Parallel()

	if hash := RootHash("   "); hash != "" {
		t.Fatalf("RootHash returned %q, want empty hash", hash)
	}
}

func TestParseVirtualPathRejectsWrongShapes(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		"zed://",
		"zed://root/session-only",
		"zed://root/channel/",
		"zed://short/channel/session",
		"zed://nothexnothexnot/channel/session",
		"zed://1234567890abcdef/0-stable /thread",
		"zed://1234567890abcdef/ 0-stable/thread",
		"zed://1234567890abcdef/0-stable/thread ",
		"zed://1234567890abcdef/channel?bad/session",
		"file:///tmp/thread",
	} {
		if _, err := ParseVirtualPath(raw); err == nil {
			t.Fatalf("ParseVirtualPath(%q) returned nil error, want invalid path error", raw)
		}
	}
}
