package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/providers/registry"
	"goodkind.io/clyde/internal/session"
)

// NewResumeCmd implements `clyde resume <title|provider-id>`. It resolves the
// argument against the clyde session store by exact visible title, provider ID,
// legacy row name, or single substring match, then shells out through the
// provider runtime with the resolved provider session id. When nothing matches,
// it forwards the raw query to the default provider runtime so upstream-native
// sessions resume transparently.
//
// `clyde -r <uuid>` and `clyde --resume <uuid>` are rewritten to this
// verb by ClassifyArgs in dispatch.go, so all three forms share one
// RunE.
func NewResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "resume <title|provider-id>",
		Short:              "Resume a clyde session by exact title or provider id",
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
				cmdResumeLog.Logger().InfoContext(ctx, "cli.resume.unknown_session.forwarding_to_provider",
					"component", "cli",
					"query", query,
				)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"No Clyde session matches %q; forwarding to the default provider.\n\n", query)
				runtime, err := registry.Default(store)
				if err != nil {
					return err
				}
				return runtime.ResumeOpaqueInteractive(ctx, session.OpaqueResumeRequest{
					Query: query,
				})
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
