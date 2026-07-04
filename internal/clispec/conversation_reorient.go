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
	Cursor        string
	MaxLines      int
	PageBytes     int
	ToolOutputs   bool
}

func (reorientInput) isClispecInput() {}

// reorientPayload is the validated input the operation's Run consumes.
type reorientPayload struct {
	ConversationID string
	WorkspaceRoot  string
	Cursor         string
	MaxLines       int
	PageBytes      int
	ToolOutputs    bool
}

func (reorientPayload) isClispecPrepared() {}

// reorientOp is the conversation reorient operation. It renders to the terminal
// command `clyde conversation reorient` and the MCP tool `clyde_reorient`. It
// returns the recovered pre-compaction transcript as bounded, cursor-paged text;
// the caller loops with --cursor until remaining is zero.
func reorientOp() Operation[reorientInput, reorientPayload] {
	return Operation[reorientInput, reorientPayload]{
		Name:       Name{Canonical: "reorient", CLIOverride: ""},
		Group:      conversationGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindValue,
		Short:      "Recover the recent transcript that /compact replaced with a summary.",
		Long:       "After /compact, recover the conversation history from before the compaction point as a dense chat plus tool-call transcript, so the agent can re-orient from the detail the summary dropped. The recovered transcript is a fixed snapshot capped to its last max_lines lines, and it is returned as bounded, cursor-paged text. Set conversation to a specific id, or set workspace to start from its newest conversation. Read every page in full and call again with the printed cursor until remaining is zero. If the response reports that the conversation was compacted again, start over from an empty cursor.",
		Examples: []string{
			"clyde conversation reorient --conversation claude:1a2b3c",
			"clyde conversation reorient --workspace ~/Sites/app",
			"clyde conversation reorient --conversation codex:019e --cursor eyJvIjozMDAwMH0",
		},
		Args: nil,
		Params: []Param[reorientInput]{
			StringParam("conversation", "Current conversation id, native id, title, or artifact path. Empty uses the newest conversation in workspace.", "", false,
				func(in *reorientInput, v string) { in.Conversation = v }),
			StringParam("workspace", "Workspace root. Required when conversation is empty.", "", false,
				func(in *reorientInput, v string) { in.WorkspaceRoot = v }),
			StringParam("cursor", "Continuation cursor from a prior page. Empty starts at the first page.", "", false,
				func(in *reorientInput, v string) { in.Cursor = v }),
			IntParam("max_lines", "Cap the recovered transcript to its last N lines. Zero uses the daemon default.", 0,
				func(in *reorientInput, v int) { in.MaxLines = v }),
			IntParam("page_bytes", "Per-page byte budget. Zero uses the daemon default that keeps a page inline.", 0,
				func(in *reorientInput, v int) { in.PageBytes = v }),
			BoolParam("tool_outputs", "Include full tool result bodies instead of the default tool-call commands.", false,
				func(in *reorientInput, v bool) { in.ToolOutputs = v }),
		},
		New: func() reorientInput {
			return reorientInput{
				Conversation:  "",
				WorkspaceRoot: "",
				Cursor:        "",
				MaxLines:      0,
				PageBytes:     0,
				ToolOutputs:   false,
			}
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       nil,
		Prepare:        prepareReorient,
		Run:            nil,
		runResult: func(ctx context.Context, p reorientPayload) (Result, error) {
			page, err := daemon.ReorientConversation(ctx, p.ConversationID, p.WorkspaceRoot, p.Cursor, p.MaxLines, p.PageBytes, p.ToolOutputs)
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
		Cursor:         strings.TrimSpace(in.Cursor),
		MaxLines:       in.MaxLines,
		PageBytes:      in.PageBytes,
		ToolOutputs:    in.ToolOutputs,
	}, nil
}
