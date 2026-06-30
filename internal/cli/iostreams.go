// Package cli is the root of the clyde CLI surface.
//
// Each subcommand lives in its own subpackage under internal/cli and
// exports a NewCmd(f *Factory) constructor. The Factory threads
// dependencies (store, config, daemon client, logger, IO streams)
// through the command tree so subcommands stay pure and testable.
package cli

import (
	"io"
	"os"
)

// IOStreams bundles the three standard streams every subcommand may
// touch. Tests inject buffers; production wires [os.Stdin] / [[os.Stdout]] /
// [os.Stderr]. Subcommands must read and write through the streams here
// rather than touching os.Std* directly so the cobra command tree
// stays drivable from tests.
type IOStreams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// SystemIOStreams returns the production wiring backed by the process
// stdin / stdout / stderr.
func SystemIOStreams() *IOStreams {
	return &IOStreams{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}
}
