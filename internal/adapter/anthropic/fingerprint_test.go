package anthropic

import (
	"regexp"
	"strings"
	"testing"
)

func TestBuildAttributionHeaderShape(t *testing.T) {
	// Wire shape from mitm: cc_version=<semver>.<3hex>; cc_entrypoint=...; cch=<5hex>;
	re := regexp.MustCompile(`^x-anthropic-billing-header: cc_version=\d+\.\d+\.\d+\.[0-9a-f]{3}; cc_entrypoint=sdk-cli; cch=[0-9a-f]{5};$`)
	for range 20 {
		got := BuildAttributionHeader("2.1.114", "sdk-cli")
		if !re.MatchString(got) {
			t.Fatalf("unexpected shape: %q", got)
		}
	}
}

func TestBuildAttributionHeaderRandomSuffixes(t *testing.T) {
	seen := map[string]struct{}{}
	for range 30 {
		line := BuildAttributionHeader("1.0.0", "sdk-cli")
		// Two calls should usually differ (extremely unlikely collision).
		seen[line] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("expected variation across calls, got %d unique of 30", len(seen))
	}
}

func TestRandomHexDeterministicLength(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 6} {
		h := randomHex(n)
		if len(h) != n {
			t.Fatalf("randomHex(%d) len=%d", n, len(h))
		}
		for _, r := range h {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("non-hex in %q", h)
			}
		}
	}
}
