package cmd

import (
	"strings"
	"testing"
)

func TestResumeUnknownSessionErrorGuidesToCanonicalAndExplicitPaths(t *testing.T) {
	err := resumeUnknownSessionError("thread-123")
	if err == nil {
		t.Fatal("resumeUnknownSessionError returned nil, want error")
	}
	message := err.Error()
	for _, want := range []string{
		"`clyde --resume`",
		"`clyde codex resume thread-123`",
		"`clyde claude --resume thread-123`",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q missing %q", message, want)
		}
	}
}
