package clispec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemon"
)

type conversationDeleteInput struct {
	ConversationID string
	Execute        bool
}

func (conversationDeleteInput) isClispecInput() {}

type conversationDeletePayload struct {
	ConversationID string
	Execute        bool
}

func (conversationDeletePayload) isClispecPrepared() {}

type conversationDeleteOutput struct {
	ConversationID string `json:"conversation_id"`
	Rows           int64  `json:"rows"`
	Deleted        bool   `json:"deleted"`
}

func (conversationDeleteOutput) isClispecStructuredPayload() {}

// conversationDeleteOp declares `clyde conversation delete`. The MCP surface is
// off, and no MCP tool deletes stored rows. The work function opens the Milvus
// client in process, as the daemon backfill commands do, and does not call the
// daemon.
func conversationDeleteOp() Operation[conversationDeleteInput, conversationDeletePayload] {
	return Operation[conversationDeleteInput, conversationDeletePayload]{
		Name:       Name{Canonical: "conversation_delete", CLIOverride: "delete"},
		Group:      conversationGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Delete the stored semantic search rows of one conversation.",
		Long:       "Delete every semantic search row of one conversation from the Milvus conversation collection: the rows with that conversationId, and the older rows with a relativePath that starts with conv/<id>/, convtool/<id>/, or convthink/<id>/. Rows of every other conversation stay, and the transcript files stay. Without --execute the command prints the number of rows it would delete and deletes no row.",
		Examples:   []string{"clyde conversation delete codex:abc", "clyde conversation delete codex:abc --execute"},
		Args: []Arg[conversationDeleteInput]{
			PositionalArg("conversation_id", "Exact id of the conversation to delete from the semantic index.",
				func(in *conversationDeleteInput, v string) { in.ConversationID = v }),
		},
		Params: []Param[conversationDeleteInput]{
			BoolParam("execute", "Delete the rows. Without this flag the command is a read-only dry-run.", false,
				func(in *conversationDeleteInput, v bool) { in.Execute = v }),
		},
		New:            func() conversationDeleteInput { return conversationDeleteInput{ConversationID: "", Execute: false} },
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       nil,
		Prepare: func(in conversationDeleteInput) (conversationDeletePayload, error) {
			conversationID := strings.TrimSpace(in.ConversationID)
			if conversationID == "" {
				return conversationDeletePayload{}, errors.New("conversation id is required")
			}
			return conversationDeletePayload{ConversationID: conversationID, Execute: in.Execute}, nil
		},
		Run: nil,
		runResult: func(ctx context.Context, payload conversationDeletePayload) (Result, error) {
			cfg, err := config.LoadGlobalOrDefault()
			if err != nil {
				return nil, logFail(ctx, surfaceFromContext(ctx), "delete_config_failed", "load config", err)
			}
			client, err := daemon.OpenConversationSearchClient(ctx, cfg.Conversation.Semantic)
			if err != nil {
				return nil, logFail(ctx, surfaceFromContext(ctx), "delete_open_failed", "open conversation semantic client", err)
			}
			defer func() { _ = client.Close(context.WithoutCancel(ctx)) }()
			collectionID := cfg.Conversation.Semantic.CollectionID
			if !payload.Execute {
				count, countErr := client.CountConversationRows(ctx, collectionID, payload.ConversationID)
				if countErr != nil {
					return nil, logFail(ctx, surfaceFromContext(ctx), "delete_count_failed", "count conversation rows", countErr)
				}
				slog.InfoContext(ctx, "clispec.conversation_delete.dry_run", "concern", "cli.conversation", "component", "clispec", "conversation_id", payload.ConversationID, "rows", count)
				return valueResult{
					Payload: conversationDeleteOutput{ConversationID: payload.ConversationID, Rows: count, Deleted: false},
					Text:    fmt.Sprintf("Would delete %d rows of conversation %s. Pass --execute to delete them.\n", count, payload.ConversationID),
				}, nil
			}
			removed, deleteErr := client.DeleteConversation(ctx, collectionID, payload.ConversationID)
			if deleteErr != nil {
				return nil, logFail(ctx, surfaceFromContext(ctx), "delete_failed", "delete conversation rows", deleteErr)
			}
			return valueResult{
				Payload: conversationDeleteOutput{ConversationID: payload.ConversationID, Rows: removed, Deleted: true},
				Text:    fmt.Sprintf("Deleted %d rows of conversation %s.\n", removed, payload.ConversationID),
			}, nil
		},
	}
}
