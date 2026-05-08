package sessionrename

import "testing"

func TestFingerprint_Stable(t *testing.T) {
	a := fingerprint(SourceFirstUserMessage, "investigate redis lag")
	b := fingerprint(SourceFirstUserMessage, "investigate redis lag")
	if a != b {
		t.Fatalf("fingerprint not stable: %q vs %q", a, b)
	}
	if len(a) != fingerprintLength {
		t.Fatalf("len(fingerprint)=%d want %d", len(a), fingerprintLength)
	}
}

func TestFingerprint_DistinctOnText(t *testing.T) {
	a := fingerprint(SourceFirstUserMessage, "redis lag")
	b := fingerprint(SourceFirstUserMessage, "redis surge")
	if a == b {
		t.Fatalf("fingerprint collided across distinct text")
	}
}

func TestFingerprint_DistinctOnKind(t *testing.T) {
	const otherKind SourceKind = "other"
	a := fingerprint(SourceFirstUserMessage, "redis lag")
	b := fingerprint(otherKind, "redis lag")
	if a == b {
		t.Fatalf("fingerprint collided across distinct kinds")
	}
}
