package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

const daemonGRPCUnixScheme = "unix://"

// DefaultDaemonGRPCAddress returns the default gRPC target for the daemon socket.
func DefaultDaemonGRPCAddress() string {
	return daemonGRPCUnixScheme + DaemonSocketPath()
}

// DaemonSocketPathFromGRPCAddress returns the Unix socket path named by a daemon gRPC target.
func DaemonSocketPathFromGRPCAddress(address string) (string, error) {
	trimmed := strings.TrimSpace(address)
	if !strings.HasPrefix(trimmed, daemonGRPCUnixScheme) {
		return "", fmt.Errorf("daemon.grpc_address must use unix://")
	}
	socketPath := strings.TrimSpace(strings.TrimPrefix(trimmed, daemonGRPCUnixScheme))
	if socketPath == "" {
		return "", fmt.Errorf("daemon.grpc_address must include a Unix socket path")
	}
	if !filepath.IsAbs(socketPath) {
		return "", fmt.Errorf("daemon.grpc_address socket path %q must be absolute", socketPath)
	}
	return filepath.Clean(socketPath), nil
}

func applyDaemonDefaultsAndValidate(daemon *DaemonConfig) error {
	if daemon == nil {
		return nil
	}
	daemon.GRPCAddress = strings.TrimSpace(daemon.GRPCAddress)
	if daemon.GRPCAddress == "" {
		daemon.GRPCAddress = DefaultDaemonGRPCAddress()
		return nil
	}
	socketPath, err := DaemonSocketPathFromGRPCAddress(daemon.GRPCAddress)
	if err != nil {
		return err
	}
	daemon.GRPCAddress = daemonGRPCUnixScheme + socketPath
	return nil
}
