package mitm

import (
	"fmt"
	"io"
	"log/slog"
	"runtime"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/mitm/truststore"
	"goodkind.io/clyde/internal/config"
)

// newTrustCmd builds the `clyde mitm trust` parent and its three
// subcommands. The Registry and config loader are resolved through
// closures so tests can inject fakes without exec-ing into the OS.
func newTrustCmd(f *cli.Factory) *cobra.Command {
	return newTrustCmdWithDeps(f, truststore.New, config.LoadGlobalOrDefault)
}

// newTrustCmdWithDeps is the test-driven constructor. registryFactory
// returns the platform Registry; loadConfig returns the merged config.
// Both are wired here rather than baked into truststore so the CLI
// can render a uniform error when the platform is unsupported and
// can keep status, install, and uninstall on the same dependency
// graph.
func newTrustCmdWithDeps(
	f *cli.Factory,
	registryFactory func() truststore.Registry,
	loadConfig func() (*config.Config, error),
) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Install or remove the Clyde MITM CA in the OS trust store",
		Long: "Trust manages the optional system-level integration that lets " +
			"GUI apps and tools that ignore env-var CA hints accept Clyde " +
			"MITM-signed certificates. Install requires admin and prompts " +
			"once via /usr/bin/sudo. Status is read-only and never mutates " +
			"the trust store.",
	}
	cmd.AddCommand(newTrustInstallCmd(f, registryFactory, loadConfig))
	cmd.AddCommand(newTrustUninstallCmd(f, registryFactory))
	cmd.AddCommand(newTrustStatusCmd(f, registryFactory, loadConfig))
	return cmd
}

func newTrustInstallCmd(
	f *cli.Factory,
	registryFactory func() truststore.Registry,
	loadConfig func() (*config.Config, error),
) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the Clyde MITM CA into the OS trust store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.trust.install.load_config_failed", "err", err)
				return fmt.Errorf("load config: %w", err)
			}
			reg := registryFactory()
			if reg.Platform() == truststore.PlatformUnsupported {
				return unsupportedPlatformError(reg)
			}
			if err := reg.Install(cfg.MITM.CA.CertPath); err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.trust.install.failed",
					"platform", string(reg.Platform()),
					"cert_path", cfg.MITM.CA.CertPath,
					"err", err)
				return fmt.Errorf("install ca: %w", err)
			}
			fmt.Fprintf(f.IOStreams.Out, "installed: %s\n", cfg.MITM.CA.CertPath)
			fmt.Fprintf(f.IOStreams.Out, "platform: %s\n", reg.Platform())
			return nil
		},
	}
	return cmd
}

func newTrustUninstallCmd(
	f *cli.Factory,
	registryFactory func() truststore.Registry,
) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the Clyde MITM CA from the OS trust store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg := registryFactory()
			if reg.Platform() == truststore.PlatformUnsupported {
				return unsupportedPlatformError(reg)
			}
			if err := reg.Uninstall(); err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.trust.uninstall.failed",
					"platform", string(reg.Platform()),
					"err", err)
				return fmt.Errorf("uninstall ca: %w", err)
			}
			fmt.Fprintf(f.IOStreams.Out, "uninstalled\n")
			fmt.Fprintf(f.IOStreams.Out, "platform: %s\n", reg.Platform())
			return nil
		},
	}
	return cmd
}

func newTrustStatusCmd(
	f *cli.Factory,
	registryFactory func() truststore.Registry,
	loadConfig func() (*config.Config, error),
) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print the OS trust-store state for the Clyde MITM CA",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.trust.status.load_config_failed", "err", err)
				return fmt.Errorf("load config: %w", err)
			}
			reg := registryFactory()
			status, statusErr := truststore.ReadStatus(reg, cfg.MITM.CA.CertPath)
			if statusErr != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.trust.status.read_failed",
					"platform", string(reg.Platform()),
					"err", statusErr)
				writeTrustStatus(f.IOStreams.Out, status, cfg.MITM.CA.CertPath, statusErr)
				return statusErr
			}
			writeTrustStatus(f.IOStreams.Out, status, cfg.MITM.CA.CertPath, nil)
			return nil
		},
	}
	return cmd
}

// writeTrustStatus renders the typed status as the human-readable
// report. The format mirrors the existing `clyde mitm status` style:
// one key per line, lowercase keys, fixed value column.
func writeTrustStatus(out io.Writer, status truststore.InstallStatus, certPath string, statusErr error) {
	fmt.Fprintf(out, "platform: %s\n", status.Platform)
	fmt.Fprintf(out, "common_name: %s\n", status.CommonName)
	fmt.Fprintf(out, "ca_cert_path: %s\n", certPath)
	fmt.Fprintf(out, "installed: %t\n", status.Installed)
	fmt.Fprintf(out, "on_disk_fingerprint: %s\n", emptyFingerprint(status.OnDiskFingerprint))
	fmt.Fprintf(out, "installed_fingerprint: %s\n", emptyFingerprint(status.InstalledFingerprint))
	fmt.Fprintf(out, "fingerprints_match: %t\n", status.FingerprintsMatch())
	if status.Detail != "" {
		fmt.Fprintf(out, "detail: %s\n", status.Detail)
	}
	if statusErr != nil {
		fmt.Fprintf(out, "error: %s\n", statusErr.Error())
	}
}

func emptyFingerprint(fp truststore.Fingerprint) string {
	if fp.IsZero() {
		return "absent"
	}
	return fp.String()
}

// unsupportedPlatformError returns a wrapped ErrUnsupportedPlatform
// that names the runtime so the operator sees both the canonical
// sentinel and the OS we are running on.
func unsupportedPlatformError(reg truststore.Registry) error {
	slog.Warn("cli.mitm.trust.unsupported_platform",
		"goos", runtime.GOOS, "registry", string(reg.Platform()))
	return fmt.Errorf("%w: build target=%s registry=%s",
		truststore.ErrUnsupportedPlatform, runtime.GOOS, reg.Platform())
}
