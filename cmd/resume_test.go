package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/session"
)

func TestResolveSessionForResumeAmbiguousMatchesShowVisibleTitles(t *testing.T) {
	store := session.NewFileStore(t.TempDir())
	first := session.NewSession("merry-swan", "uuid-1")
	first.Metadata.DisplayTitle = "Merry Swan"
	second := session.NewSession("quiet-swan", "uuid-2")
	second.Metadata.DisplayTitle = "Quiet Swan"
	for _, sess := range []*session.Session{first, second} {
		if err := store.Create(sess); err != nil {
			t.Fatalf("create session %q: %v", sess.Name, err)
		}
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	got, err := resolveSessionForResume(cmd, store, "Swan")

	if got != nil {
		t.Fatalf("resolved session = %#v, want nil ambiguous result", got)
	}
	if err == nil {
		t.Fatalf("resolve error = nil, want ambiguity error")
	}
	if !strings.Contains(err.Error(), "exact visible title or provider session id") {
		t.Fatalf("resolve error = %q, want visible-title guidance", err.Error())
	}
	output := out.String()
	if !strings.Contains(output, "Merry Swan (uuid-1)") || !strings.Contains(output, "Quiet Swan (uuid-2)") {
		t.Fatalf("ambiguous output missing visible titles:\n%s", output)
	}
	if strings.Contains(output, "merry-swan") || strings.Contains(output, "quiet-swan") {
		t.Fatalf("ambiguous output leaked legacy row names:\n%s", output)
	}
}
