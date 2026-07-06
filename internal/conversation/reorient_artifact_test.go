package conversation

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// artifactTestParser resolves any path to a fixed Claude record and streams a
// fixed message set, so RenderReorientArtifact can be tested without a real
// provider transcript and without touching the index cache.
type artifactTestParser struct {
	messages []transcript.Message
}

func (artifactTestParser) Provider() providerid.Provider { return providerid.ProviderClaude }

func (artifactTestParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return nil, nil
}

func (artifactTestParser) ScanRecord(path string, stamp FileStamp) (Record, bool) {
	return Record{
		ID:           "claude:artifact",
		Provider:     providerid.ProviderClaude,
		NativeID:     "artifact",
		ArtifactPath: path,
		ArtifactKind: "transcript",
		SizeBytes:    stamp.Size,
	}, true
}

func (p artifactTestParser) Stream(string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		for _, message := range p.messages {
			if !yield(message, nil) {
				return
			}
		}
	}
}

func TestRenderReorientArtifactReadsPathWithoutIndex(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write temp transcript: %v", err)
	}
	registry := NewRegistry()
	registry.Register(artifactTestParser{messages: []transcript.Message{
		{Role: "user", Text: "recover-me-marker", Visibility: transcript.MessageVisibilityVisible},
		{Role: "assistant", Text: "acknowledged", Visibility: transcript.MessageVisibilityVisible},
	}})
	// NewIndex builds an unscanned index: RenderReorientArtifact must read the
	// path directly through the parser rather than consult cached records.
	index := NewIndex(registry)
	body, err := index.RenderReorientArtifact(path, providerid.ProviderClaude, ReorientOptions{
		SyntheticPreCompact: true,
		IncludeToolOutputs:  true,
	})
	if err != nil {
		t.Fatalf("RenderReorientArtifact err = %v", err)
	}
	if !strings.Contains(body, "recover-me-marker") {
		t.Fatalf("rendered body missing the recovered message; got %q", body)
	}
}

func TestRenderReorientArtifactUnknownProvider(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write temp transcript: %v", err)
	}
	index := NewIndex(NewRegistry())
	if _, err := index.RenderReorientArtifact(path, providerid.ProviderClaude, ReorientOptions{SyntheticPreCompact: true}); err == nil {
		t.Fatal("expected an error when no registered provider parses the artifact")
	}
}

func TestRenderReorientArtifactMissingFile(t *testing.T) {
	t.Parallel()
	index := NewIndex(NewRegistry())
	missing := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	if _, err := index.RenderReorientArtifact(missing, providerid.ProviderClaude, ReorientOptions{SyntheticPreCompact: true}); err == nil {
		t.Fatal("expected an error for a missing transcript file")
	}
}
