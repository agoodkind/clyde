package mitm

import (
	"fmt"
	"log/slog"
	"net"
	"slices"
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
		Short: "Print each configured MITM listen address, CA paths, and listener liveness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.status.load_config_failed", "concern", "cli.mitm", "err", err)
				return fmt.Errorf("load config: %w", err)
			}
			out := f.IOStreams.Out
			ids := make([]string, 0, len(cfg.MITM.Listeners))
			for id := range cfg.MITM.Listeners {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			for _, id := range ids {
				listener := cfg.MITM.Listeners[id]
				for _, address := range mitmListenAddresses(listener) {
					conn, dialErr := dial("tcp", address, statusListenerDialTimeout)
					listenerUp := dialErr == nil
					if conn != nil {
						_ = conn.Close()
					}
					fmt.Fprintf(out, "listener[%s] listen_address: %s\n", id, address)
					fmt.Fprintf(out, "listener[%s] listener_up: %t\n", id, listenerUp)
				}
			}
			fmt.Fprintf(out, "ca_cert_path: %s\n", cfg.MITM.CA.CertPath)
			fmt.Fprintf(out, "ca_key_path: %s\n", cfg.MITM.CA.KeyPath)
			return nil
		},
	}
	return cmd
}

// mitmListenAddresses expands one listener into the loopback addresses the
// daemon binds: a "localhost" listener binds both [::1] and 127.0.0.1 (mirroring
// the daemon's mitmBindAddrs), and any explicit host binds one address. Probing
// each socket keeps a half-bound listener from reading as healthy.
func mitmListenAddresses(listener config.MITMListenerConfig) []string {
	if strings.TrimSpace(listener.Host) == "localhost" {
		port := strconv.Itoa(listener.Port)
		return []string{net.JoinHostPort("::1", port), net.JoinHostPort("127.0.0.1", port)}
	}
	return []string{mitmListenAddress(listener)}
}

// mitmListenAddress unwraps a bracketed IPv6 host so JoinHostPort yields one
// set of brackets, mirroring the daemon's mitmListenAddr derivation.
func mitmListenAddress(listener config.MITMListenerConfig) string {
	host := strings.TrimSpace(listener.Host)
	if len(host) >= 2 && strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		inner := strings.TrimSpace(host[1 : len(host)-1])
		if strings.Contains(inner, ":") {
			host = inner
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(listener.Port))
}
