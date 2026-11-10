package cli

import "github.com/spf13/cobra"

// verbose is set via the persistent --verbose / -v flag on the root
// command. Subcommands read it through Factory.Verbose() so the
// global stays internal to this package.
var verbose bool

// copyOutput is set via the persistent --copy flag on the root command.
// It is a global capability: when set, the shared output layer copies the
// command's body to the clipboard and reports the line count, so no
// command implements copy itself. Subcommands read it through
// Factory.Copy().
var copyOutput bool

// RegisterGlobalFlags attaches the persistent flags every subcommand
// inherits. Called once from cmd/clyde/main.go when assembling the root
// command tree; not used by subcommand packages (they read values via
// the Factory instead of touching package globals directly).
func RegisterGlobalFlags(root *cobra.Command) {
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")
	root.PersistentFlags().BoolVar(&copyOutput, "copy", false, "Copy the command's output body to the clipboard (macOS only; errors on other platforms)")
}
