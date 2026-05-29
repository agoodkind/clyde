package cli

import "github.com/spf13/cobra"

// verbose is set via the persistent --verbose / -v flag on the root
// command. Subcommands read it through Factory.Verbose() so the
// global stays internal to this package.
var verbose bool

// RegisterGlobalFlags attaches the persistent flags every subcommand
// inherits. Called once from cmd/clyde/main.go when assembling the root
// command tree; not used by subcommand packages (they read values via
// the Factory instead of touching package globals directly).
func RegisterGlobalFlags(root *cobra.Command) {
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")
}
