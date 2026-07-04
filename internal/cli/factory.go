package cli

import (
	"log/slog"

	"goodkind.io/clyde/internal/config"
)

// BuildInfo carries the ldflag-injected version metadata so the
// version subcommand and the slog initialiser can both surface it
// without reading package-level globals.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// Factory threads dependencies through the cobra command tree.
//
// Each subpackage under internal/cli receives a *Factory in its
// NewCmd(f *Factory) constructor and resolves the dependencies it
// needs lazily through the closures here. Lazy resolution keeps cobra
// startup cheap and lets tests swap in fakes without touching every
// subcommand.
type Factory struct {
	IOStreams *IOStreams
	Logger    *slog.Logger
	Build     BuildInfo

	// Verbose returns true when the user passed --verbose / -v on
	// the root command. Subcommands consult this to decide whether
	// to print extra diagnostic detail to IOStreams.Out.
	Verbose func() bool

	// Copy returns true when the user passed --copy on the root
	// command. The shared output layer consults this to copy the
	// command's body to the clipboard; no command reads it directly.
	Copy func() bool

	// Config loads the merged global+project configuration. Returns
	// the zero value with no error when no config file exists so
	// subcommands can rely on the defaults without special casing.
	Config func() (*config.Config, error)
}

// NewSystemFactory wires the production dependencies. Tests build
// their own Factory with stub closures.
func NewSystemFactory(build BuildInfo) *Factory {
	return &Factory{
		IOStreams: SystemIOStreams(),
		Logger:    slog.Default(),
		Build:     build,
		Verbose:   func() bool { return verbose },
		Copy:      func() bool { return copyOutput },
		Config:    config.LoadGlobalOrDefault,
	}
}
