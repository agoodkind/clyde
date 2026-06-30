package clispec

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

// TestRenderCobraNestsGroups asserts a group chain renders as nested terminal
// parents, so an operation under a child group sits two levels below the root.
func TestRenderCobraNestsGroups(t *testing.T) {
	t.Parallel()
	outer := &Group{Use: "outer", Short: "outer group", Long: "", Parent: nil}
	inner := &Group{Use: "inner", Short: "inner group", Long: "", Parent: outer}

	op := probeOp()
	op.Group = inner

	reg := &Registry{ops: nil, handwritten: nil}
	Register(reg, op)

	var out bytes.Buffer
	roots := RenderCobra(reg, testFactory(&out))
	if len(roots) != 1 {
		t.Fatalf("roots: got %d, want 1", len(roots))
	}
	if roots[0].Use != "outer" {
		t.Fatalf("root Use: got %q, want outer", roots[0].Use)
	}

	innerCmd, _, err := roots[0].Find([]string{"inner"})
	if err != nil {
		t.Fatalf("find inner under outer: %v", err)
	}
	if innerCmd.Use != "inner" {
		t.Fatalf("inner Use: got %q, want inner", innerCmd.Use)
	}

	leaf, _, err := roots[0].Find([]string{"inner", "probe-op"})
	if err != nil {
		t.Fatalf("find leaf under inner: %v", err)
	}
	if leaf.Name() != "probe-op" {
		t.Fatalf("leaf name: got %q, want probe-op", leaf.Name())
	}
}

func TestRenderCobraMergesGroupedOpsIntoHandwrittenRoot(t *testing.T) {
	t.Parallel()

	group := &Group{Use: "shared", Short: "shared group", Long: "", Parent: nil}
	op := probeOp()
	op.Group = group

	reg := &Registry{ops: nil, handwritten: nil}
	Register(reg, op)
	reg.AddHandwritten(HandwrittenCommand{
		Build: func(_ *cli.Factory) *cobra.Command {
			cmd := &cobra.Command{Use: "shared"}
			cmd.AddCommand(&cobra.Command{Use: "handwritten"})
			return cmd
		},
	})

	var out bytes.Buffer
	roots := RenderCobra(reg, testFactory(&out))
	if len(roots) != 1 {
		t.Fatalf("roots: got %d, want 1", len(roots))
	}

	shared := roots[0]
	if shared.Name() != "shared" {
		t.Fatalf("root name: got %q, want shared", shared.Name())
	}
	if _, _, err := shared.Find([]string{"probe-op"}); err != nil {
		t.Fatalf("find clispec child under shared root: %v", err)
	}
	if _, _, err := shared.Find([]string{"handwritten"}); err != nil {
		t.Fatalf("find handwritten child under shared root: %v", err)
	}
}
