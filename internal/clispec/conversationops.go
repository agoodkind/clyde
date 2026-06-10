package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"

	"goodkind.io/clyde/internal/clock"
	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/homedir"
	"goodkind.io/clyde/internal/providerid"
)

type listConversationsInput struct {
	Limit           int
	Offset          int
	Provider        string
	WorkspaceRoot   string
	Query           string
	IncludeArchived bool
	All             bool
}

type getConversationInput struct {
	ConversationID string
	LastN          int
}

type getContextInput struct {
	ConversationID string
	Timestamp      string
	MessageIndex   int
	Before         int
	After          int
}

type searchConversationInput struct {
	ConversationID string
	Query          string
	Limit          int
	Roles          string
	After          string
	Until          string
	MinScore       float64
}

type searchConversationsInput struct {
	Query                string
	Limit                int
	Provider             string
	WorkspaceRoot        string
	IncludeArchived      bool
	Roles                string
	After                string
	Until                string
	MinScore             float64
	PerConversationLimit int
}

type searchStatusInput struct {
	ResultID string
}

type searchCancelInput struct {
	ResultID string
}

type analyzeResultsInput struct {
	ResultID string
	Prompt   string
}

type exportInput struct {
	ConversationID string
	Options        conv.ExportOptions
	OutputPath     string
}

func (listConversationsInput) isClispecInput()   {}
func (getConversationInput) isClispecInput()     {}
func (getContextInput) isClispecInput()          {}
func (searchConversationInput) isClispecInput()  {}
func (searchConversationsInput) isClispecInput() {}
func (searchStatusInput) isClispecInput()        {}
func (searchCancelInput) isClispecInput()        {}
func (analyzeResultsInput) isClispecInput()      {}
func (exportInput) isClispecInput()              {}

// conversationGroup is the terminal parent for conversation operations.
var conversationGroup = &Group{
	Use:     "conversation",
	Short:   "Inspect Claude and Codex conversations",
	Long:    "Inspect indexed Claude and Codex conversations: list and show transcripts, read a context window around a point, search across or within conversations, and export a transcript. Clyde reads provider-owned artifacts and never mutates them.",
	Example: "clyde conversation list --limit 20\nclyde conversation export claude:1a2b3c",
	Parent:  nil,
}

// searchGroup gathers the conversation-search package commands beneath the
// conversation parent.
var searchGroup = &Group{
	Use:     "search",
	Short:   "Search conversation text",
	Long:    "Search conversation transcripts: scan across conversations for candidate ids, start an async search within one conversation, poll or cancel a running search, and analyze cached results.",
	Example: "clyde conversation search across \"auth timeout\"\nclyde conversation search within claude:1a2b3c \"auth timeout\"",
	Parent:  conversationGroup,
}

var exportFormatValues = []string{
	string(conv.ExportFormatMarkdown),
	string(conv.ExportFormatHTML),
	string(conv.ExportFormatJSON),
	string(conv.ExportFormatPlainText),
}

var whitespaceValues = []string{
	string(conv.WhitespacePreserve),
	string(conv.WhitespaceTidy),
	string(conv.WhitespaceCompact),
	string(conv.WhitespaceDense),
}

// listConversationsOp prints one filtered page of Claude and Codex
// conversation metadata.
func listConversationsOp() Operation[listConversationsInput] {
	allParam := BoolParam("all", "Return every matched conversation on the CLI.", false,
		func(in *listConversationsInput, v bool) { in.All = v })
	allParam.CLIOnly = true
	return Operation[listConversationsInput]{
		Name:     Name{Canonical: "list_conversations", CLIOverride: "list"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "List Claude and Codex conversations.",
		Long:     "List one filtered page of indexed Claude and Codex conversation metadata.",
		Examples: []string{"clyde conversation list --limit 20 --query auth"},
		Args:     nil,
		Params: []Param[listConversationsInput]{
			IntParam("limit", "Maximum conversations to return.", conv.DefaultListLimit,
				func(in *listConversationsInput, v int) { in.Limit = v }),
			IntParam("offset", "Zero-based result offset.", 0,
				func(in *listConversationsInput, v int) { in.Offset = v }),
			StringParam("provider", "Provider filter, such as claude or codex.", "", false,
				func(in *listConversationsInput, v string) { in.Provider = v }),
			StringParam("workspace", "Workspace root filter.", "", false,
				func(in *listConversationsInput, v string) { in.WorkspaceRoot = v }),
			StringParam("query", "Metadata query over ids, title, workspace, artifact path, kind, provider, and model.", "", false,
				func(in *listConversationsInput, v string) { in.Query = v }),
			BoolParam("include_archived", "Include archived conversations.", false,
				func(in *listConversationsInput, v bool) { in.IncludeArchived = v }),
			allParam,
		},
		New: func() listConversationsInput {
			return listConversationsInput{
				Limit:           conv.DefaultListLimit,
				Offset:          0,
				Provider:        "",
				WorkspaceRoot:   "",
				Query:           "",
				IncludeArchived: false,
				All:             false,
			}
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Run: func(ctx context.Context, in listConversationsInput, surface Surface, sink ResultSink) error {
			options, err := listOptionsFromInput(in)
			if err != nil {
				return logFail(ctx, surface, "list_failed", "list conversations", err)
			}
			result, err := daemon.ListConversations(ctx, options)
			if err != nil {
				return logFail(ctx, surface, "list_failed", "list conversations", err)
			}
			return sink.Text(formatListResult(result))
		},
	}
}

// getConversationOp prints a conversation transcript as plain text.
func getConversationOp() Operation[getConversationInput] {
	return Operation[getConversationInput]{
		Name:     Name{Canonical: "get_conversation", CLIOverride: "show"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Show plain text from a conversation.",
		Long:     "Print one conversation transcript as plain text. Resolve the conversation by id, native id, title, or artifact path.",
		Examples: []string{"clyde conversation show claude:1a2b3c --last-n 20"},
		Args: []Arg[getConversationInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *getConversationInput, v string) { in.ConversationID = v }),
		},
		Params: []Param[getConversationInput]{
			IntParam("last_n", "Only return the last N messages.", 0,
				func(in *getConversationInput, v int) { in.LastN = v }),
		},
		New:            func() getConversationInput { return getConversationInput{ConversationID: "", LastN: 0} },
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Run: func(ctx context.Context, in getConversationInput, surface Surface, sink ResultSink) error {
			text, err := daemon.GetConversation(ctx, in.ConversationID, in.LastN)
			if err != nil {
				return logFail(ctx, surface, "get_failed", "get conversation", err)
			}
			return sink.Text(text)
		},
	}
}

// getContextOp prints the messages around a point in a conversation.
func getContextOp() Operation[getContextInput] {
	return Operation[getContextInput]{
		Name:     Name{Canonical: "get_context", CLIOverride: "context"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Get messages around a point in a conversation.",
		Long:     "Print the messages surrounding a center point chosen by timestamp or by message index, with a configurable number of messages before and after.",
		Examples: []string{"clyde conversation context claude:1a2b3c --message-index 42 --before 5 --after 5"},
		Args: []Arg[getContextInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *getContextInput, v string) { in.ConversationID = v }),
		},
		Params: []Param[getContextInput]{
			StringParam("timestamp", "Timestamp to center on.", "", false,
				func(in *getContextInput, v string) { in.Timestamp = v }),
			IntParam("message_index", "0-based message index to center on.", -1,
				func(in *getContextInput, v int) { in.MessageIndex = v }),
			IntParam("before", "Messages before the center.", 5,
				func(in *getContextInput, v int) { in.Before = v }),
			IntParam("after", "Messages after the center.", 5,
				func(in *getContextInput, v int) { in.After = v }),
		},
		New: func() getContextInput {
			return getContextInput{ConversationID: "", Timestamp: "", MessageIndex: -1, Before: 5, After: 5}
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Run: func(ctx context.Context, in getContextInput, surface Surface, sink ResultSink) error {
			text, err := daemon.GetConversationContext(ctx, in.ConversationID, in.Timestamp, in.MessageIndex, in.Before, in.After)
			if err != nil {
				return logFail(ctx, surface, "context_failed", "get conversation context", err)
			}
			return sink.Text(text)
		},
	}
}

// searchConversationsOp scans transcript text across conversations and returns
// bounded candidate conversation ids with first-match snippets.
func searchConversationsOp() Operation[searchConversationsInput] {
	return Operation[searchConversationsInput]{
		Name:     Name{Canonical: "conversations_search", CLIOverride: "across"},
		Group:    searchGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Search across conversations for candidate ids.",
		Long:     "Search transcript text across filtered conversations and return bounded candidate conversation ids with first-match snippets.",
		Examples: []string{"clyde conversation search across \"auth timeout\" --limit 10"},
		Args: []Arg[searchConversationsInput]{
			PositionalArg("query", "Text query to find in transcript messages.",
				func(in *searchConversationsInput, v string) { in.Query = v }),
		},
		Params: []Param[searchConversationsInput]{
			IntParam("limit", "Maximum matching conversations to return.", conv.DefaultSearchLimit,
				func(in *searchConversationsInput, v int) { in.Limit = v }),
			StringParam("provider", "Provider filter, such as claude or codex.", "", false,
				func(in *searchConversationsInput, v string) { in.Provider = v }),
			StringParam("workspace", "Workspace root filter.", "", false,
				func(in *searchConversationsInput, v string) { in.WorkspaceRoot = v }),
			BoolParam("include_archived", "Include archived conversations.", false,
				func(in *searchConversationsInput, v bool) { in.IncludeArchived = v }),
			StringParam("roles", "Comma-separated message roles to keep, such as user or assistant.", "", false,
				func(in *searchConversationsInput, v string) { in.Roles = v }),
			StringParam("after", "Keep messages at or after this time (RFC3339 or YYYY-MM-DD).", "", false,
				func(in *searchConversationsInput, v string) { in.After = v }),
			StringParam("until", "Keep messages before this time (RFC3339 or YYYY-MM-DD).", "", false,
				func(in *searchConversationsInput, v string) { in.Until = v }),
			FloatParam("min_score", "Drop hits scoring below this relevance floor.", 0,
				func(in *searchConversationsInput, v float64) { in.MinScore = v }),
			IntParam("per_conversation_limit", "Cap hits per conversation; zero means uncapped.", 0,
				func(in *searchConversationsInput, v int) { in.PerConversationLimit = v }),
		},
		New: func() searchConversationsInput {
			return searchConversationsInput{
				Query:                "",
				Limit:                conv.DefaultSearchLimit,
				Provider:             "",
				WorkspaceRoot:        "",
				IncludeArchived:      false,
				Roles:                "",
				After:                "",
				Until:                "",
				MinScore:             0,
				PerConversationLimit: 0,
			}
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Run: func(ctx context.Context, in searchConversationsInput, surface Surface, sink ResultSink) error {
			options, err := searchConversationsOptionsFromInput(in)
			if err != nil {
				return logFail(ctx, surface, "search_conversations_failed", "search conversations", err)
			}
			result, err := daemon.SearchConversations(ctx, options)
			if err != nil {
				return logFail(ctx, surface, "search_conversations_failed", "search conversations", err)
			}
			return sink.Text(formatSearchConversationsResult(result))
		},
	}
}

// searchConversationOp starts an async search within one conversation and
// returns a result_id. The MCP tool is task-augmentable: a Tasks-capable
// client that supplies task params runs it to completion and gets the result
// through tasks/result, while a plain call (CLI or a non-Tasks client) returns
// the result_id immediately and polls search status.
func searchConversationOp() Operation[searchConversationInput] {
	return Operation[searchConversationInput]{
		Name:     Name{Canonical: "search_conversation", CLIOverride: "within"},
		Group:    searchGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Start an async search within one conversation and return a result_id.",
		Long:     "Start a semantic search over one conversation's indexed messages, with a literal scan covering content the index does not hold yet, and return a result_id immediately. Poll conversation search status with the result_id for the result, then pass the result_id to conversation search analyze for LLM analysis. A Tasks-capable MCP client can instead run this as a task and receive the result through tasks/result.",
		Examples: []string{"clyde conversation search within claude:1a2b3c \"auth timeout\" --roles user --limit 10"},
		Args: []Arg[searchConversationInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *searchConversationInput, v string) { in.ConversationID = v }),
			PositionalArg("query", "Natural language query.",
				func(in *searchConversationInput, v string) { in.Query = v }),
		},
		Params: []Param[searchConversationInput]{
			IntParam("limit", "Maximum matching messages to return.", 0,
				func(in *searchConversationInput, v int) { in.Limit = v }),
			StringParam("roles", "Comma-separated message roles to keep, such as user or assistant.", "", false,
				func(in *searchConversationInput, v string) { in.Roles = v }),
			StringParam("after", "Keep messages at or after this time (RFC3339 or YYYY-MM-DD).", "", false,
				func(in *searchConversationInput, v string) { in.After = v }),
			StringParam("until", "Keep messages before this time (RFC3339 or YYYY-MM-DD).", "", false,
				func(in *searchConversationInput, v string) { in.Until = v }),
			FloatParam("min_score", "Drop hits scoring below this relevance floor.", 0,
				func(in *searchConversationInput, v float64) { in.MinScore = v }),
		},
		New: func() searchConversationInput {
			return searchConversationInput{ConversationID: "", Query: "", Limit: 0, Roles: "", After: "", Until: "", MinScore: 0}
		},
		MCPTaskSupport: mcp.TaskSupportOptional,
		Run: func(ctx context.Context, in searchConversationInput, surface Surface, sink ResultSink) error {
			opts, err := withinSearchOptionsFromInput(in)
			if err != nil {
				return logFail(ctx, surface, "search_failed", "search conversation", err)
			}
			text, err := daemon.SearchConversation(ctx, in.ConversationID, in.Query, opts)
			if err != nil {
				return logFail(ctx, surface, "search_failed", "search conversation", err)
			}
			return sink.Text(text)
		},
		MCPTaskRun: func(ctx context.Context, in searchConversationInput, sink ResultSink) error {
			opts, err := withinSearchOptionsFromInput(in)
			if err != nil {
				return logFail(ctx, SurfaceMCP, "search_task_failed", "search conversation task", err)
			}
			text, err := daemon.SearchToCompletion(ctx, in.ConversationID, in.Query, opts)
			if err != nil {
				return logFail(ctx, SurfaceMCP, "search_task_failed", "search conversation task", err)
			}
			return sink.Text(text)
		},
	}
}

// searchStatusOp reports the state, progress, and result of an async search job.
func searchStatusOp() Operation[searchStatusInput] {
	return Operation[searchStatusInput]{
		Name:     Name{Canonical: "search_status", CLIOverride: "status"},
		Group:    searchGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Get the status, progress, and result of an async search.",
		Long:     "Report the state (pending, running, complete, failed, or canceled), the live progress, and, once complete, the matching excerpts for a search started by conversation search within.",
		Examples: []string{"clyde conversation search status 7f3e2d10"},
		Args: []Arg[searchStatusInput]{
			PositionalArg("result_id", "The result_id returned by search.",
				func(in *searchStatusInput, v string) { in.ResultID = v }),
		},
		Params:         nil,
		New:            func() searchStatusInput { return searchStatusInput{ResultID: ""} },
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Run: func(ctx context.Context, in searchStatusInput, surface Surface, sink ResultSink) error {
			text, err := daemon.GetSearchStatus(ctx, in.ResultID)
			if err != nil {
				return logFail(ctx, surface, "search_status_failed", "get search status", err)
			}
			return sink.Text(text)
		},
	}
}

// searchCancelOp cancels a running async search job.
func searchCancelOp() Operation[searchCancelInput] {
	return Operation[searchCancelInput]{
		Name:     Name{Canonical: "search_cancel", CLIOverride: "cancel"},
		Group:    searchGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Cancel a running async search.",
		Long:     "Cancel a search started by conversation search within, freeing the local model, and report the resulting status.",
		Examples: []string{"clyde conversation search cancel 7f3e2d10"},
		Args: []Arg[searchCancelInput]{
			PositionalArg("result_id", "The result_id returned by search.",
				func(in *searchCancelInput, v string) { in.ResultID = v }),
		},
		Params:         nil,
		New:            func() searchCancelInput { return searchCancelInput{ResultID: ""} },
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Run: func(ctx context.Context, in searchCancelInput, surface Surface, sink ResultSink) error {
			text, err := daemon.CancelSearch(ctx, in.ResultID)
			if err != nil {
				return logFail(ctx, surface, "search_cancel_failed", "cancel search", err)
			}
			return sink.Text(text)
		},
	}
}

// analyzeResultsOp runs the local analysis model over cached search results.
func analyzeResultsOp() Operation[analyzeResultsInput] {
	return Operation[analyzeResultsInput]{
		Name:     Name{Canonical: "analyze_results", CLIOverride: "analyze"},
		Group:    searchGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Analyze cached results from clyde_search_conversation.",
		Long:     "Run the analysis model over a cached search result set named by result_id, following the instruction in prompt.",
		Examples: []string{"clyde conversation search analyze 7f3e2d10 \"summarize the decisions made\""},
		Params:   nil,
		Args: []Arg[analyzeResultsInput]{
			PositionalArg("result_id", "The result_id returned by clyde_search_conversation.",
				func(in *analyzeResultsInput, v string) { in.ResultID = v }),
			PositionalArg("prompt", "Analysis instruction.",
				func(in *analyzeResultsInput, v string) { in.Prompt = v }),
		},
		New:            func() analyzeResultsInput { return analyzeResultsInput{ResultID: "", Prompt: ""} },
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Run: func(ctx context.Context, in analyzeResultsInput, surface Surface, sink ResultSink) error {
			text, err := daemon.AnalyzeSearchResults(ctx, in.ResultID, in.Prompt)
			if err != nil {
				return logFail(ctx, surface, "analyze_failed", "analyze results", err)
			}
			return sink.Text(text)
		},
	}
}

// exportTranscriptOp exports a conversation transcript. The terminal can write
// the body to a file via --output; the MCP tool returns the body as text.
func exportTranscriptOp() Operation[exportInput] {
	outputPathParam := StringParam("output", "write output to path", "", false,
		func(in *exportInput, v string) { in.OutputPath = v })
	outputPathParam.CLIOnly = true

	return Operation[exportInput]{
		Name:     Name{Canonical: "export_transcript", CLIOverride: "export"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Export a conversation transcript.",
		Long:     "Export one conversation transcript in the chosen format. The terminal always writes an artifact file and reports the written path; the MCP tool returns the body as text.",
		Examples: []string{
			"clyde conversation export claude:1a2b3c --format markdown --output transcript.md",
			"clyde conversation export claude:1a2b3c --no-thinking --no-tool-calls",
		},
		Args: []Arg[exportInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *exportInput, v string) { in.ConversationID = v }),
		},
		Params: []Param[exportInput]{
			EnumParam("format", "markdown, html, json, or plain_text.", string(conv.ExportFormatMarkdown), exportFormatValues,
				func(in *exportInput, v string) { in.Options.Format = conv.ExportFormat(v) }),
			EnumParam("whitespace", "preserve, tidy, compact, or dense.", string(conv.WhitespacePreserve), whitespaceValues,
				func(in *exportInput, v string) { in.Options.Whitespace = conv.WhitespaceMode(v) }),
			outputPathParam,
			IntParam("history_start", "First message index to include.", 0,
				func(in *exportInput, v int) { in.Options.HistoryStart = v }),
			// Default-on content types get a presence off-switch: the flag's
			// presence sets v=true, which the setter inverts to exclude the type.
			// This avoids the pflag `--flag=false` form needed to turn a
			// default-true boolean off.
			BoolParam("no_chat", "Exclude conversation chat text (included by default).", false,
				func(in *exportInput, v bool) { in.Options.IncludeChat = !v }),
			BoolParam("no_thinking", "Exclude assistant thinking blocks (included by default).", false,
				func(in *exportInput, v bool) { in.Options.IncludeThinking = !v }),
			BoolParam("no_tool_calls", "Exclude tool calls (included by default).", false,
				func(in *exportInput, v bool) { in.Options.IncludeToolCalls = !v }),
			// Default-off content types get a presence on-switch: the flag's
			// presence sets v=true, which the setter maps straight to inclusion.
			BoolParam("with_tool_outputs", "Include tool result bodies (excluded by default).", false,
				func(in *exportInput, v bool) { in.Options.IncludeToolOutputs = v }),
			BoolParam("with_system_prompts", "Include system-injected prompts (excluded by default).", false,
				func(in *exportInput, v bool) { in.Options.IncludeSystemPrompts = v }),
			BoolParam("with_system_messages", "Include provider system transcript records (excluded by default).", false,
				func(in *exportInput, v bool) { in.Options.IncludeSystemMessages = v }),
			BoolParam("with_raw_json_metadata", "Include JSON metadata fields (excluded by default).", false,
				func(in *exportInput, v bool) { in.Options.IncludeRawJSONMetadata = v }),
		},
		New: func() exportInput {
			return exportInput{
				ConversationID: "",
				OutputPath:     "",
				Options: conv.ExportOptions{
					Format:                 conv.ExportFormatMarkdown,
					HistoryStart:           0,
					Whitespace:             conv.WhitespacePreserve,
					IncludeChat:            true,
					IncludeThinking:        true,
					IncludeSystemPrompts:   false,
					IncludeSystemMessages:  false,
					IncludeToolCalls:       true,
					IncludeToolOutputs:     false,
					IncludeRawJSONMetadata: false,
				},
			}
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Run: func(ctx context.Context, in exportInput, surface Surface, sink ResultSink) error {
			body, err := daemon.ExportTranscript(ctx, in.ConversationID, in.Options)
			if err != nil {
				return logFail(ctx, surface, "export_failed", "export transcript", err)
			}
			if surface == SurfaceCLI {
				path := in.OutputPath
				if path == "" {
					path = defaultExportOutputPath(in.ConversationID, in.Options.Format)
				}
				if err := sink.WriteFile(path, body); err != nil {
					slog.WarnContext(ctx, "cli.conversation.export_write_failed", "concern", "cli.conversation", "component", "cli", "path", path, "err", err)
					return fmt.Errorf("export transcript: write output %s: %w", path, err)
				}
				return sink.Text("wrote: " + path + "\n")
			}
			return sink.Bytes(body)
		},
	}
}

func listOptionsFromInput(in listConversationsInput) (conv.ListOptions, error) {
	provider, err := providerFilter(in.Provider)
	if err != nil {
		return conv.ListOptions{}, err
	}
	return conv.ListOptions{
		Limit:           in.Limit,
		Offset:          in.Offset,
		Provider:        provider,
		WorkspaceRoot:   cleanWorkspaceRoot(in.WorkspaceRoot),
		Query:           in.Query,
		IncludeArchived: in.IncludeArchived,
		All:             in.All,
	}, nil
}

func searchConversationsOptionsFromInput(in searchConversationsInput) (conv.SearchConversationsOptions, error) {
	provider, err := providerFilter(in.Provider)
	if err != nil {
		return conv.SearchConversationsOptions{}, err
	}
	fromUnix, err := timeBoundUnix(in.After, "after")
	if err != nil {
		return conv.SearchConversationsOptions{}, err
	}
	untilUnix, err := timeBoundUnix(in.Until, "until")
	if err != nil {
		return conv.SearchConversationsOptions{}, err
	}
	return conv.SearchConversationsOptions{
		Query:                in.Query,
		Limit:                in.Limit,
		Provider:             provider,
		WorkspaceRoot:        cleanWorkspaceRoot(in.WorkspaceRoot),
		IncludeArchived:      in.IncludeArchived,
		Roles:                splitRoles(in.Roles),
		FromUnix:             fromUnix,
		UntilUnix:            untilUnix,
		MinScore:             in.MinScore,
		PerConversationLimit: in.PerConversationLimit,
	}, nil
}

func withinSearchOptionsFromInput(in searchConversationInput) (conv.WithinSearchOptions, error) {
	fromUnix, err := timeBoundUnix(in.After, "after")
	if err != nil {
		return conv.WithinSearchOptions{}, err
	}
	untilUnix, err := timeBoundUnix(in.Until, "until")
	if err != nil {
		return conv.WithinSearchOptions{}, err
	}
	return conv.WithinSearchOptions{
		Limit:     in.Limit,
		Roles:     splitRoles(in.Roles),
		FromUnix:  fromUnix,
		UntilUnix: untilUnix,
		MinScore:  in.MinScore,
	}, nil
}

// splitRoles parses the comma-separated roles flag into the role set, dropping
// empty entries.
func splitRoles(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	roles := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			roles = append(roles, trimmed)
		}
	}
	return roles
}

// timeBoundLayouts are the accepted spellings for the after and until flags.
var timeBoundLayouts = []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"}

// timeBoundUnix parses one time flag to a unix timestamp. Empty means
// unbounded and returns zero. Naive spellings (no zone) are interpreted in the
// host's current zone, since the flag describes the operator's wall clock.
func timeBoundUnix(raw string, flagName string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	hostZone := clock.Now().Location()
	for _, layout := range timeBoundLayouts {
		if parsed, err := time.ParseInLocation(layout, trimmed, hostZone); err == nil {
			return parsed.Unix(), nil
		}
	}
	return 0, fmt.Errorf("parse %s %q: want RFC3339, YYYY-MM-DD HH:MM, or YYYY-MM-DD", flagName, trimmed)
}

func providerFilter(raw string) (conv.Provider, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return providerid.ProviderUnspecified, nil
	}
	provider, ok := providerid.Parse(raw)
	if !ok || !provider.Valid() {
		return providerid.ProviderUnspecified, fmt.Errorf("unsupported provider %q", raw)
	}
	return provider, nil
}

func cleanWorkspaceRoot(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	expanded := homedir.Expand(raw)
	absolute, err := filepath.Abs(expanded)
	if err == nil {
		expanded = absolute
	}
	return filepath.Clean(expanded)
}

func formatListResult(result conv.ListResult) string {
	var out strings.Builder
	fmt.Fprintf(&out, "total_matched: %d\n", result.TotalMatched)
	fmt.Fprintf(&out, "returned_count: %d\n", result.ReturnedCount)
	fmt.Fprintf(&out, "offset: %d\n", result.Offset)
	fmt.Fprintf(&out, "limit: %d\n", result.Limit)
	fmt.Fprintf(&out, "next_offset: %d\n", result.NextOffset)
	fmt.Fprintf(&out, "has_more: %t\n", result.HasMore)
	if len(result.Records) == 0 {
		out.WriteString("\nNo conversations found.\n")
		return out.String()
	}
	out.WriteString("\n")
	writeConversationRecordHeader(&out)
	for _, record := range result.Records {
		writeConversationRecordRow(&out, record)
	}
	return out.String()
}

func formatSearchConversationsResult(result conv.SearchConversationsResult) string {
	var out strings.Builder
	if result.Warming {
		out.WriteString("Semantic index is warming; showing live results.\n")
	}
	fmt.Fprintf(&out, "returned_count: %d\n", result.ReturnedCount)
	fmt.Fprintf(&out, "limit: %d\n", result.Limit)
	fmt.Fprintf(&out, "conversations_scanned: %d\n", result.ConversationsScanned)
	fmt.Fprintf(&out, "has_more: %t\n", result.HasMore)
	if len(result.Matches) == 0 {
		out.WriteString("\nNo matching conversations found.\n")
		return out.String()
	}
	out.WriteString("\n")
	out.WriteString("conversation_id\tprovider\tnative_id\ttitle\tworkspace_root\tartifact_path\tartifact_kind\tmodel\tcreated_at\tupdated_at\tsize_bytes\tarchived\tmessage_index\trole\ttimestamp\tsnippet\n")
	for _, match := range result.Matches {
		record := match.Record
		fmt.Fprintf(
			&out,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%t\t%d\t%s\t%s\t%s\n",
			tsvField(record.ID),
			tsvField(record.Provider.String()),
			tsvField(record.NativeID),
			tsvField(record.Title),
			tsvField(shortPath(record.WorkspaceRoot)),
			tsvField(record.ArtifactPath),
			tsvField(record.ArtifactKind),
			tsvField(record.Model),
			formatTime(record.CreatedAt),
			formatTime(record.UpdatedAt),
			record.SizeBytes,
			record.Archived,
			match.MessageIndex,
			tsvField(match.Role),
			formatTime(match.Timestamp),
			tsvField(match.Snippet),
		)
	}
	return out.String()
}

func writeConversationRecordHeader(out *strings.Builder) {
	out.WriteString("conversation_id\tprovider\tnative_id\ttitle\tworkspace_root\tartifact_path\tartifact_kind\tmodel\tcreated_at\tupdated_at\tsize_bytes\tarchived\n")
}

func writeConversationRecordRow(out *strings.Builder, record conv.Record) {
	fmt.Fprintf(
		out,
		"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%t\n",
		tsvField(record.ID),
		tsvField(record.Provider.String()),
		tsvField(record.NativeID),
		tsvField(record.Title),
		tsvField(shortPath(record.WorkspaceRoot)),
		tsvField(record.ArtifactPath),
		tsvField(record.ArtifactKind),
		tsvField(record.Model),
		formatTime(record.CreatedAt),
		formatTime(record.UpdatedAt),
		record.SizeBytes,
		record.Archived,
	)
}

func tsvField(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// logFail emits a surface-correct warning event and returns the wrapped error.
// The terminal surfaces the returned error; the MCP adapter returns it as tool
// text.
func logFail(ctx context.Context, surface Surface, shortEvent, operation string, err error) error {
	switch surface {
	case SurfaceMCP:
		slog.WarnContext(ctx, "mcp.conversation."+shortEvent, "concern", "mcp.server.context", "component", "mcpserver", "operation", operation, "err", err)
	case SurfaceCLI:
		fallthrough
	default:
		slog.WarnContext(ctx, "cli.conversation."+shortEvent, "concern", "cli.conversation", "component", "cli", "operation", operation, "err", err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func shortPath(path string) string {
	return homedir.Contract(path)
}

func defaultExportOutputPath(conversationID string, format conv.ExportFormat) string {
	base := sanitizeExportBasename(conversationID)
	if base == "" {
		base = "transcript"
	}
	return base + exportExtension(format)
}

func sanitizeExportBasename(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var out []rune
	lastDash := false
	for _, r := range trimmed {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			out = append(out, r)
			lastDash = false
		case r == '.', r == '_':
			out = append(out, r)
			lastDash = false
		default:
			if !lastDash {
				out = append(out, '-')
				lastDash = true
			}
		}
	}
	sanitized := strings.Trim(string(out), "-")
	if sanitized == "" {
		return ""
	}
	return filepath.Clean(sanitized)
}

func exportExtension(format conv.ExportFormat) string {
	switch format {
	case conv.ExportFormatHTML:
		return ".html"
	case conv.ExportFormatJSON:
		return ".json"
	case conv.ExportFormatPlainText:
		return ".txt"
	case conv.ExportFormatMarkdown, "":
		fallthrough
	default:
		return ".md"
	}
}
