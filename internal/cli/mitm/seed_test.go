package mitm

import (
	"bytes"
	"testing"

	"goodkind.io/clyde/internal/cli"
)

// TestSeedBaselineCommandRequiresUpstream verifies the command marks --upstream
// required, so cobra rejects a missing value before the run step reaches the
// daemon.
func TestSeedBaselineCommandRequiresUpstream(t *testing.T) {
	out := &bytes.Buffer{}
	factory := &cli.Factory{IOStreams: &cli.IOStreams{Out: out, Err: &bytes.Buffer{}, In: &bytes.Buffer{}}}

	cmd := newBaselineSeedCmd(factory)
	cmd.SetArgs([]string{"--from", "/does/not/matter.jsonl"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --upstream is missing")
	}
}
