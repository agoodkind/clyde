package cli

import "github.com/spf13/cobra"

// fullHelpTemplate is the single source of truth for help rendering. It is
// installed as the usage template so that both `--help` and Cobra's
// usage-on-error path produce identical output: the command description, the
// usage line, examples, available subcommands, local flags, and inherited
// global flags. It is adapted from spf13/cobra's default usage template
// (command.go UsageTemplate) with the leading description block of the default
// help template prepended, and the alias, command-group, and help-topic
// branches removed because this CLI uses none of them.
const fullHelpTemplate = `{{with (or .Long .Short)}}{{. | trimTrailingWhitespaces}}

{{end}}Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

// helpDelegatesToUsage makes the help template render the usage string, so a
// `--help` request and a usage-on-error both flow through fullHelpTemplate.
// One template, two entry points, no drift.
const helpDelegatesToUsage = `{{.UsageString}}`

// InstallHelpRendering wires the declarative help behavior onto an assembled
// command tree. It must be called after every command has been added to root,
// because it walks the whole tree. It does three things, none of which inspect
// any error:
//
//  1. Installs fullHelpTemplate as the usage template and points the help
//     template at it, so `--help` and usage-on-error render the same full help.
//     Cobra resolves templates by walking to the parent, so setting them once
//     on root covers every subcommand.
//  2. Wraps every command's run function to set SilenceUsage just before the
//     command body runs. Bad flags, bad args, and unknown subcommands all fail
//     before that line, so Cobra renders full help; a failure inside the body
//     happens after, so help is suppressed and only the error shows. The
//     boundary is positional and never reads the error.
func InstallHelpRendering(root *cobra.Command) {
	root.SetUsageTemplate(fullHelpTemplate)
	root.SetHelpTemplate(helpDelegatesToUsage)
	silenceUsageAfterValidation(root)
}

// silenceUsageAfterValidation walks the command tree and decorates each
// command's run function so the first thing it does is set SilenceUsage. A
// command reaches its run function only after Cobra has parsed flags and
// validated arguments, so any error before that point still carries
// SilenceUsage=false and Cobra prints the full-help usage; any error from the
// body itself carries SilenceUsage=true and Cobra prints only the error.
func silenceUsageAfterValidation(cmd *cobra.Command) {
	for _, child := range cmd.Commands() {
		silenceUsageAfterValidation(child)
	}
	if cmd.RunE != nil {
		originalRunE := cmd.RunE
		cmd.RunE = func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			return originalRunE(c, args)
		}
		return
	}
	if cmd.Run != nil {
		originalRun := cmd.Run
		cmd.Run = func(c *cobra.Command, args []string) {
			c.SilenceUsage = true
			originalRun(c, args)
		}
	}
}
