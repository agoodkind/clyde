package searchacceptance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/searchacceptance"
)

func TestVerifySnapshotRejectsChangedSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "conversation.jsonl")
	content := []byte("original transcript\x00with source bytes\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := searchacceptance.Digest(content) + "  ./conversation.jsonl\n"
	manifestPath := filepath.Join(root, "MANIFEST.sha256")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := searchacceptance.Digest([]byte(manifest))
	result, err := searchacceptance.VerifySnapshot(t.Context(), root, manifestPath, digest)
	if err != nil || result.Files != 1 || result.Bytes != int64(len(content)) || result.ManifestDigest != digest {
		t.Fatalf("frozen source verification failed: %+v, %v", result, err)
	}
	if err := os.WriteFile(path, []byte("changed transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := searchacceptance.VerifySnapshot(t.Context(), root, manifestPath, digest); err == nil {
		t.Fatal("changed source was accepted")
	}
}

func TestVerifySnapshotRejectsManifestAndPathChanges(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	content := []byte("external transcript")
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.jsonl")); err != nil {
		t.Fatal(err)
	}
	fileDigest := searchacceptance.Digest(content)
	for _, entry := range []string{
		fileDigest + "  ./escape.jsonl\n",
		fileDigest + "  ../outside.jsonl\n",
		fileDigest + "  " + outside + "\n",
		"invalid manifest\n",
		"",
	} {
		manifestPath := filepath.Join(root, "MANIFEST.sha256")
		if err := os.WriteFile(manifestPath, []byte(entry), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := searchacceptance.VerifySnapshot(t.Context(), root, manifestPath, searchacceptance.Digest([]byte(entry))); err == nil {
			t.Fatalf("invalid source path or manifest was accepted: %q", entry)
		}
	}
	manifestPath := filepath.Join(root, "MANIFEST.sha256")
	if _, err := searchacceptance.VerifySnapshot(t.Context(), root, manifestPath, searchacceptance.Digest([]byte("different manifest"))); err == nil {
		t.Fatal("changed manifest was accepted")
	}
}

func TestVerifySnapshotRejectsRepeatedSourceAndCancellation(t *testing.T) {
	root := t.TempDir()
	content := []byte("source")
	if err := os.WriteFile(filepath.Join(root, "source.jsonl"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	entry := searchacceptance.Digest(content) + "  ./source.jsonl\n"
	manifestPath := filepath.Join(root, "MANIFEST.sha256")
	if err := os.WriteFile(manifestPath, []byte(entry+entry), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := searchacceptance.VerifySnapshot(t.Context(), root, manifestPath, searchacceptance.Digest([]byte(entry+entry))); err == nil {
		t.Fatal("repeated source was accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := searchacceptance.VerifySnapshot(ctx, root, manifestPath, searchacceptance.Digest([]byte(entry+entry))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verification returned %v", err)
	}
}
