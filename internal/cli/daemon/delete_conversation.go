package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/response"
)

// newDeleteConversationCmd builds the operator command that deletes the stored
// rows of one conversation from the Milvus conversation collection. The command
// runs only when the operator passes the conversation id, and it deletes no row
// of any other conversation.
func newDeleteConversationCmd(f *cli.Factory) *cobra.Command {
	conversationID := ""
	cmd := &cobra.Command{
		Use:     "delete-conversation",
		Short:   "Delete the stored semantic rows of one conversation",
		Long:    "Delete every row of one conversation from the Milvus conversation collection: the rows with that conversationId, and the older rows with a relativePath that starts with conv/<id>/, convtool/<id>/, or convthink/<id>/. Rows of every other conversation stay. The --conversation flag is required.",
		Example: "clyde daemon delete-conversation --conversation codex:abc",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeleteConversation(cmd.Context(), f, conversationID)
		},
	}
	cmd.Flags().StringVar(&conversationID, "conversation", "", "Conversation id to delete from the semantic index. Required.")
	return cmd
}

func runDeleteConversation(ctx context.Context, f *cli.Factory, conversationID string) error {
	trimmedConversationID := strings.TrimSpace(conversationID)
	if trimmedConversationID == "" {
		return errors.New("--conversation must name the conversation id to delete")
	}
	cfg, err := f.Config()
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.delete_conversation.config_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("load config: %w", err)
	}
	client, err := daemonsvc.OpenConversationSearchClient(ctx, cfg.Conversation.Semantic)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.delete_conversation.open_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("open conversation semantic client: %w", err)
	}
	defer func() { _ = client.Close(context.WithoutCancel(ctx)) }()
	removed, err := client.DeleteConversation(ctx, cfg.Conversation.Semantic.CollectionID, trimmedConversationID)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.delete_conversation.failed", "concern", "cli.daemon", "component", "cli", "conversation_id", trimmedConversationID, "err", err)
		return fmt.Errorf("delete conversation %s: %w", trimmedConversationID, err)
	}
	if writeErr := response.WriteResult(ctx, f.IOStreams.Out, f.IOStreams.Err, fmt.Sprintf(
		"Deleted %d rows of conversation %s.\n", removed, trimmedConversationID,
	)); writeErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.delete_conversation.write_failed", "concern", "cli.daemon", "component", "cli", "err", writeErr)
		return fmt.Errorf("write delete result: %w", writeErr)
	}
	return nil
}
