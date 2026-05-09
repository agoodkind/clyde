package daemon

import (
	"fmt"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	claudelifecycle "goodkind.io/clyde/internal/providers/claude/lifecycle"
)

func newLaunchRemoteWorkerCmd(_ *cli.Factory) *cobra.Command {
	var sessionName string
	var sessionID string
	var basedir string
	var incognito bool

	cmd := &cobra.Command{
		Use:    "launch-remote-worker",
		Short:  "Run a daemon-owned headless remote-control Claude session",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sessionName == "" || sessionID == "" {
				return fmt.Errorf("session-name and session-id are required")
			}
			env := map[string]string{
				"CLYDE_SESSION_NAME": sessionName,
				"CLYDE_SESSION_ID":   sessionID,
			}
			cliDaemonLog.Logger().Info("cli.daemon.launch_remote_worker.invoked",
				"component", "cli",
				"session", sessionName,
				"session_id", sessionID,
				"basedir", basedir,
				"incognito", incognito,
			)
			return claudelifecycle.StartHeadlessRemoteWorker(env, "", basedir, sessionID)
		},
	}
	cmd.Flags().StringVar(&sessionName, "session-name", "", "exact clyde display name")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "pre-assigned Claude session UUID")
	cmd.Flags().StringVar(&basedir, "basedir", "", "working directory for the launched session")
	cmd.Flags().BoolVar(&incognito, "incognito", false, "ephemeral launch")
	return cmd
}
