package clispec

import (
	"context"
	"fmt"
	"strings"

	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
)

// reorientInput is the raw input of the conversation reorient operation. Every
// field is an optional flag; Prepare requires at least a conversation or a
// workspace to resolve the current conversation from.
type reorientInput struct {
	Conversation  string
	WorkspaceRoot string
	Topic         string
	Cursor        string
	Window        int
	Limit         int
	PageBytes     int
}

func (reorientInput) isClispecInput() {}

// reorientPayload is the validated input the operation's Run consumes.
type reorientPayload struct {
	ConversationID string
	WorkspaceRoot  string
	Topic          string
	Cursor         string
	Window         int
	Limit          int
	PageBytes      int
}

func (reorientPayload) isClispecPrepared() {}

// reorientOp is the conversation reorient operation. It renders to the terminal
// command `clyde conversation reorient` and the MCP tool `clyde_reorient`. It
// resolves the post-fork, post-compaction recovery context and returns one
// cursor-paged page of evidence; the caller loops with --cursor until remaining
// is zero.
func reorientOp() Operation[reorientInput, reorientPayload] {
	return Operation[reorientInput, reorientPayload]{
		Name:       Name{Canonical: "reorient", CLIOverride: ""},
		Group:      conversationGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindValue,
		Short:      "Rebuild post-compaction recovery context as paged evidence.",
		Long:       "Resolve the recovery context for a conversation after a fork and a compaction, and return it as bounded, cursor-paged evidence. Reorient walks backward deterministically: it reads the latest compaction checkpoint, resolves the fork parent through conversation lineage, falls back to the same-conversation checkpoint when there was no fork, and enriches with project memory and a workspace-scoped search. Set conversation to a specific id, or set workspace to start from its newest conversation. Each page stays small enough to read inline; call again with the printed cursor until remaining is zero before reasoning.",
		Examples: []string{
			"clyde conversation reorient --conversation claude:1a2b3c",
			"clyde conversation reorient --workspace ~/Sites/app --topic \"auth retry\"",
			"clyde conversation reorient --conversation codex:019e --cursor eyJvZmZzZXQiOjN9",
		},
		Args: nil,
		Params: []Param[reorientInput]{
			StringParam("conversation", "Current conversation id, native id, title, or artifact path. Empty uses the newest conversation in workspace.", "", false,
				func(in *reorientInput, v string) { in.Conversation = v }),
			StringParam("workspace", "Workspace root. Required when conversation is empty; also scopes memory and the fallback search.", "", false,
				func(in *reorientInput, v string) { in.WorkspaceRoot = v }),
			StringParam("topic", "Topic to narrow memory docs and the fallback search.", "", false,
				func(in *reorientInput, v string) { in.Topic = v }),
			StringParam("cursor", "Continuation cursor from a prior page. Empty starts at the first page.", "", false,
				func(in *reorientInput, v string) { in.Cursor = v }),
			IntParam("window", "Messages before and after each rendered context window.", conv.DefaultReorientWindow,
				func(in *reorientInput, v int) { in.Window = v }),
			IntParam("limit", "Maximum memory and fallback-search evidence items.", conv.DefaultReorientSearchLimit,
				func(in *reorientInput, v int) { in.Limit = v }),
			IntParam("page_bytes", "Per-page byte budget. Zero uses the daemon default that keeps a page inline.", 0,
				func(in *reorientInput, v int) { in.PageBytes = v }),
		},
		New: func() reorientInput {
			return reorientInput{
				Conversation:  "",
				WorkspaceRoot: "",
				Topic:         "",
				Cursor:        "",
				Window:        conv.DefaultReorientWindow,
				Limit:         conv.DefaultReorientSearchLimit,
				PageBytes:     0,
			}
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       nil,
		Prepare:        prepareReorient,
		Run:            nil,
		runResult: func(ctx context.Context, p reorientPayload) (Result, error) {
			page, err := daemon.ReorientConversation(ctx, p.ConversationID, p.WorkspaceRoot, p.Topic, p.Cursor, p.Window, p.Limit, p.PageBytes)
			if err != nil {
				return nil, logFail(ctx, surfaceFromContext(ctx), "reorient_failed", "reorient conversation", err)
			}
			text := conv.RenderReorientPageText(page)
			return valueResult{
				Payload: reorientPageOutputFromDomain(page),
				Text:    text,
			}, nil
		},
	}
}

// prepareReorient validates the raw input and requires a conversation or a
// workspace to resolve the current conversation from.
func prepareReorient(in reorientInput) (reorientPayload, error) {
	conversation := strings.TrimSpace(in.Conversation)
	workspace := cleanWorkspaceRoot(in.WorkspaceRoot)
	if conversation == "" && workspace == "" {
		return reorientPayload{}, fmt.Errorf("reorient requires --conversation or --workspace")
	}
	return reorientPayload{
		ConversationID: conversation,
		WorkspaceRoot:  workspace,
		Topic:          strings.TrimSpace(in.Topic),
		Cursor:         strings.TrimSpace(in.Cursor),
		Window:         in.Window,
		Limit:          in.Limit,
		PageBytes:      in.PageBytes,
	}, nil
}
