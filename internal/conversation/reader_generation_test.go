package conversation

import (
	"strings"
	"testing"
	"time"
)

func generationTestStamp() FileStamp {
	return FileStamp{Size: 4096, Mtime: time.Unix(1710000000, 0)}
}

func generationTestRecord(artifactKind string) Record {
	return Record{
		ID:           "cursor:one",
		Provider:     ProviderCursor,
		ArtifactKind: artifactKind,
	}
}

// TestContentFingerprintChangesWhenAReaderRenumbersMessages is the stale-index
// fix. A reader change that renumbers a conversation's messages leaves every
// stored message index pointing at a different turn, and the artifact's bytes do
// not change, so the file stamp alone would never re-advertise it and a finished
// transcript would never be re-fed.
func TestContentFingerprintChangesWhenAReaderRenumbersMessages(t *testing.T) {
	t.Parallel()

	stamp := generationTestStamp()
	got := ContentFingerprint(generationTestRecord(string(ArtifactKindCursorAgentTranscript)), stamp)

	if got == stamp.Fingerprint() {
		t.Fatalf("fingerprint = %q, want it to differ from the bare stamp %q", got, stamp.Fingerprint())
	}
	if !strings.HasPrefix(got, stamp.Fingerprint()) {
		t.Fatalf("fingerprint = %q, want it to still carry the stamp", got)
	}
}

// TestContentFingerprintLeavesUnchangedReadersAlone proves the re-feed is
// targeted at the one store whose reader changed. Cursor's composer and
// legacy-chat stores have their own readers and untouched numbering, so keying
// the generation by provider would re-embed them for nothing.
func TestContentFingerprintLeavesUnchangedReadersAlone(t *testing.T) {
	t.Parallel()

	stamp := generationTestStamp()
	unchanged := []string{
		"cursor_composer",
		"cursor_background_composer",
		"cursor_legacy_chat",
		"transcript",
		"rollout",
	}
	for _, artifactKind := range unchanged {
		if got := ContentFingerprint(generationTestRecord(artifactKind), stamp); got != stamp.Fingerprint() {
			t.Fatalf("%s fingerprint = %q, want the bare stamp %q", artifactKind, got, stamp.Fingerprint())
		}
	}
}

// TestContentFingerprintStillTracksTheArtifact proves the generation does not
// replace the stamp: a conversation that grows is still re-advertised.
func TestContentFingerprintStillTracksTheArtifact(t *testing.T) {
	t.Parallel()

	record := generationTestRecord(string(ArtifactKindCursorAgentTranscript))
	before := ContentFingerprint(record, generationTestStamp())
	grown := ContentFingerprint(record, FileStamp{Size: 8192, Mtime: time.Unix(1710000600, 0)})

	if before == grown {
		t.Fatalf("fingerprint = %q for two different stamps, want them to differ", before)
	}
}
