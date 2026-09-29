package daemon

import (
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

// newReconcileEmbeddedConversationCmd builds the operator command that
// reconciles one conversation between the Clyde outbox and the embedded
// search library.
func newReconcileEmbeddedConversationCmd(f *cli.Factory) *cobra.Command {
	conversationID := ""
	cmd := &cobra.Command{
		Use:   "reconcile-embedded-conversation",
		Short: "Rebuild one conversation's embedded search outbox state from the library",
		Long: "Abort the unfinished generations of one conversation, rebuild its committed fields and owner metadata from the rows " +
			"the embedded search library published, and clear its blocked state. The daemon applies the current conversation " +
			"metadata through ReprojectScalars on its next pass. Requires conversation.semantic.backend = \"embedded\".",
		Example: "clyde daemon reconcile-embedded-conversation --conversation codex:019de9aa-3a00-7010-bd9f-a6ee71559357",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return daemonsvc.ReconcileEmbeddedConversation(cmd.Context(), f.IOStreams.Out, strings.TrimSpace(conversationID))
		},
	}
	cmd.Flags().StringVar(&conversationID, "conversation", "", "Conversation id to reconcile.")
	_ = cmd.MarkFlagRequired("conversation")
	return cmd
}
