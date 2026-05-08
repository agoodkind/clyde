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
