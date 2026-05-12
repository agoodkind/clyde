package output

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"
)

// PersistentFlag registers --output-format on the root command. The
// flag is persistent so every subcommand inherits it without any
// additional wiring. Subcommands that never emit structured data
// (interactive launchers, the TUI dashboard) ignore the flag; the
// flag is a no-op until a subcommand opts in via From.
func PersistentFlag(root *cobra.Command) {
	root.PersistentFlags().String(FlagName, string(FormatText),
		"Output format: text (default, human-readable) or json")
}

// From reads the inherited --output-format flag from cmd and returns
// a configured *Encoder backed by w. Subcommands call this once per
// RunE invocation. An unknown value produces a typed
// *UnknownFormatError from ParseFormat.
func From(cmd *cobra.Command, w io.Writer) (*Encoder, error) {
	raw, err := cmd.Flags().GetString(FlagName)
	if err != nil {
		slog.Warn("output.factory.read_flag_failed",
			"component", "cli",
			"subcomponent", "output",
			"flag", FlagName,
			"err", err,
		)
		return nil, fmt.Errorf("output: read flag: %w", err)
	}
	format, err := ParseFormat(raw)
	if err != nil {
		return nil, err
	}
	return &Encoder{Format: format, W: w}, nil
}
