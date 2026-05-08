package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewResumeCmd implements `clyde resume <name|uuid>`. It resolves the
// argument against the clyde session store (by name, UUID, display
// name, or fuzzy match) and shells out through the provider runtime with the
// resolved provider session id. When nothing matches, it fails with guidance
// toward the dashboard selector or an explicit provider override rather than
// silently assuming one provider.
//
// `clyde -r <uuid>` and `clyde --resume <uuid>` are rewritten to this
// verb by ClassifyArgs in dispatch.go, so all three forms share one
// RunE.
func NewResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "resume <name|uuid>",
		Short:              "Resolve a clyde session name to its provider session id and resume it",
		Args:               cobra.ExactArgs(1),
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := newCommandContext("cli.resume")
			query := args[0]
			cmdResumeLog.Logger().InfoContext(ctx, "cli.resume.invoked",
				"component", "cli",
				"query", query,
			)
			store, err := globalStore(ctx)
			if err != nil {
				return err
			}
			sess, err := resolveSessionForResume(cmd, store, query)
			if err != nil {
				return err
			}
			if sess == nil {
				cmdResumeLog.Logger().WarnContext(ctx, "cli.resume.unknown_session",
					"component", "cli",
					"query", query,
				)
				return resumeUnknownSessionError(query)
			}
			cmdResumeLog.Logger().InfoContext(ctx, "cli.resume.resolved",
				"component", "cli",
				"query", query,
				"session", sess.Name,
				"session_id", sess.Metadata.ProviderSessionID(),
			)
			return resumeSession(ctx, sess, store, false)
		},
	}
}

func resumeUnknownSessionError(query string) error {
	return fmt.Errorf(
		"session %q was not found in clyde; use `clyde --resume` to pick a stored session, `clyde codex resume %s` for an unmanaged Codex thread, or `clyde claude --resume %s` for an unmanaged Claude session",
		query,
		query,
		query,
	)
}
