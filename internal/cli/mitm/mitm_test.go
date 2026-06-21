package mitm

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/cli"
)

func TestNewCmdRegistersMitmParent(t *testing.T) {
	cmd := NewCmd(testFactory())
	if cmd == nil {
		t.Fatal("NewCmd returned nil")
	}
	if cmd.Name() != "mitm" {
		t.Fatalf("cmd.Name = %q, want mitm", cmd.Name())
	}

	var childNames []string
	for _, child := range cmd.Commands() {
		childNames = append(childNames, child.Name())
	}
	sort.Strings(childNames)
	if strings.Join(childNames, ",") != "baseline,trust" {
		t.Fatalf("mitm handwritten children: got %v, want [baseline trust]", childNames)
	}
	trustCmd, _, err := cmd.Find([]string{"trust"})
	if err != nil {
		t.Fatalf("find trust: %v", err)
	}
	if trustCmd == nil || trustCmd.Name() != "trust" {
		t.Fatalf("trust subcommand missing; got %v", trustCmd)
	}
	baselineCmd, _, err := cmd.Find([]string{"baseline"})
	if err != nil {
		t.Fatalf("find baseline: %v", err)
	}
	if baselineCmd == nil || baselineCmd.Name() != "baseline" {
		t.Fatalf("baseline subcommand missing; got %v", baselineCmd)
	}
	seedCmd, _, err := cmd.Find([]string{"baseline", "seed"})
	if err != nil {
		t.Fatalf("find baseline seed: %v", err)
	}
	if seedCmd == nil || seedCmd.Name() != "seed" {
		t.Fatalf("baseline seed subcommand missing; got %v", seedCmd)
	}
}

func testFactory() *cli.Factory {
	return &cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  &bytes.Buffer{},
			Out: &bytes.Buffer{},
			Err: &bytes.Buffer{},
		},
	}
}
