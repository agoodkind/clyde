package mitm

import (
	"bytes"
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
	statusCmd, _, err := cmd.Find([]string{"status"})
	if err != nil {
		t.Fatalf("find status: %v", err)
	}
	if statusCmd == nil || statusCmd.Name() != "status" {
		t.Fatalf("status subcommand missing; got %v", statusCmd)
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
