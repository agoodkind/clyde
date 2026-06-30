package output

import (
	"github.com/spf13/cobra"
)

// PersistentFlag registers --output-format on the root command. The flag is
// persistent so every subcommand inherits it without any additional wiring. A
// subcommand reads the resolved value with ParseFormat and chooses how to
// render its reply; the daemon performs the rendering for daemon-owned queries.
func PersistentFlag(root *cobra.Command) {
	root.PersistentFlags().String(FlagName, string(FormatText),
		"Output format: text (default, human-readable) or json")
}
