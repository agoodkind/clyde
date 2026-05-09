package truststore

import (
	"fmt"
	"log/slog"
)

// ReadStatus is the read-only entry point used by the CLI status
// subcommand. The wrapper exists so the surface area called from the
// status code path is provably narrower than the surface area called
// from install or uninstall: an AST-style test in
// truststore_status_lints_test.go confirms this file does not import
// any mutating helpers.
//
// The function delegates to Registry.Status, which the platform
// contract requires to be read-only. Keeping a separate exported
// entry point lets callers choose the read path explicitly without
// needing the mutating Install or Uninstall handles in scope.
func ReadStatus(reg Registry, certPath string) (InstallStatus, error) {
	status, err := reg.Status(certPath)
	if err != nil {
		slog.Warn("cli.mitm.truststore.read_status_failed",
			"platform", string(reg.Platform()),
			"cert_path", certPath,
			"err", err,
		)
		return status, fmt.Errorf("cli.mitm.truststore: read status on %s: %w", reg.Platform(), err)
	}
	return status, nil
}
