package mitm

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
)

const statusListenerDialTimeout = 200 * time.Millisecond

// statusDialer matches [net.DialTimeout]. Tests inject a fake to keep the probe
// hermetic and to simulate a closed listener without leaking sockets.
type statusDialer func(network, address string, timeout time.Duration) (net.Conn, error)

func newStatusCmd(f *cli.Factory) *cobra.Command {
	return newStatusCmdWithDialer(f, net.DialTimeout, config.LoadGlobalOrDefault)
}

func newStatusCmdWithDialer(f *cli.Factory, dial statusDialer, loadConfig func() (*config.Config, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print the configured MITM listen address, CA paths, and listener liveness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.status.load_config_failed", "concern", "cli.mitm", "err", err)
				return fmt.Errorf("load config: %w", err)
			}
			address := mitmListenAddress(cfg.MITM.Listen)
			conn, dialErr := dial("tcp", address, statusListenerDialTimeout)
			listenerUp := dialErr == nil
			if conn != nil {
				_ = conn.Close()
			}
			out := f.IOStreams.Out
			fmt.Fprintf(out, "listen_address: %s\n", address)
			fmt.Fprintf(out, "ca_cert_path: %s\n", cfg.MITM.CA.CertPath)
			fmt.Fprintf(out, "ca_key_path: %s\n", cfg.MITM.CA.KeyPath)
			fmt.Fprintf(out, "listener_up: %t\n", listenerUp)
			return nil
		},
	}
	return cmd
}

// mitmListenAddress unwraps a bracketed IPv6 host so JoinHostPort yields one
// set of brackets, mirroring the daemon's mitmListenAddr derivation.
func mitmListenAddress(listen config.MITMListenConfig) string {
	host := strings.TrimSpace(listen.Host)
	if len(host) >= 2 && strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		inner := strings.TrimSpace(host[1 : len(host)-1])
		if strings.Contains(inner, ":") {
			host = inner
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(listen.Port))
}
