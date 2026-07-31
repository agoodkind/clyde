package conversation

import (
	"testing"
	"time"
)

func fingerprintTestStamp() FileStamp {
	return FileStamp{Size: 4096, Mtime: time.Unix(1710000000, 0)}
}

func fingerprintTestRecord(artifactKind string) Record {
	return Record{
		ID:           "cursor:one",
		Provider:     ProviderCursor,
		ArtifactKind: artifactKind,
	}
}

func TestContentFingerprintPreservesCursorCompatibilitySuffix(t *testing.T) {
	t.Parallel()

	stamp := fingerprintTestStamp()
	got := ContentFingerprint(fingerprintTestRecord(string(ArtifactKindCursorAgentTranscript)), stamp)
	want := stamp.Fingerprint() + ":r1"
	if got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
}

func TestContentFingerprintLeavesOtherArtifactKindsUnchanged(t *testing.T) {
	t.Parallel()

	stamp := fingerprintTestStamp()
	unchanged := []string{
		"cursor_composer",
		"cursor_background_composer",
		"cursor_legacy_chat",
		"transcript",
		"rollout",
	}
	for _, artifactKind := range unchanged {
		if got := ContentFingerprint(fingerprintTestRecord(artifactKind), stamp); got != stamp.Fingerprint() {
			t.Fatalf("%s fingerprint = %q, want the bare stamp %q", artifactKind, got, stamp.Fingerprint())
		}
	}
}

// TestContentFingerprintStillTracksTheArtifact proves the compatibility suffix
// does not replace the stamp: a conversation that grows is re-advertised.
func TestContentFingerprintStillTracksTheArtifact(t *testing.T) {
	t.Parallel()

	record := fingerprintTestRecord(string(ArtifactKindCursorAgentTranscript))
	before := ContentFingerprint(record, fingerprintTestStamp())
	grown := ContentFingerprint(record, FileStamp{Size: 8192, Mtime: time.Unix(1710000600, 0)})

	if before == grown {
		t.Fatalf("fingerprint = %q for two different stamps, want them to differ", before)
	}
}
