package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/homedir"
)

// listConversationsInput is the input vehicle for the zero-input list
// operation. It is a generic type argument for a real operation, not a wire or
// config payload, so the empty struct is intentional here.
type listConversationsInput struct{}

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
	Depth          string
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

func (listConversationsInput) isClispecInput()  {}
func (getConversationInput) isClispecInput()    {}
func (getContextInput) isClispecInput()         {}
func (searchConversationInput) isClispecInput() {}
func (analyzeResultsInput) isClispecInput()     {}
func (exportInput) isClispecInput()             {}

// conversationGroup is the terminal parent the six conversation operations
// attach under. The MCP tool names stay verbose; only the terminal grouping
// and short verbs come from here.
var conversationGroup = &Group{
	Use:    "conversation",
	Short:  "Inspect Claude and Codex conversations",
	Long:   "",
	Parent: nil,
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

// listConversationsOp prints every Claude and Codex conversation. The terminal
// prints tab-separated rows; the MCP tool returns a counted bullet list.
func listConversationsOp() Operation[listConversationsInput] {
	return Operation[listConversationsInput]{
		Name:     Name{Canonical: "list_conversations", CLIOverride: "list"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "List Claude and Codex conversations.",
		Long:     "List every indexed Claude and Codex conversation, one per line, as id, provider, native id, workspace, and title.",
		Examples: []string{"clyde conversation list"},
		Args:     nil,
		Params:   nil,
		New:      func() listConversationsInput { return listConversationsInput{} },
		Run: func(ctx context.Context, _ listConversationsInput, surface Surface, sink ResultSink) error {
			records, err := daemon.ListConversations(ctx)
			if err != nil {
				return logFail(ctx, surface, "list_failed", "list conversations", err)
			}
			if len(records) == 0 {
				return sink.Text("No conversations found.")
			}
			var out strings.Builder
			if surface == SurfaceMCP {
				fmt.Fprintf(&out, "%d conversations:\n\n", len(records))
				for _, record := range records {
					fmt.Fprintf(&out, "- %s [%s] %s", record.ID, record.Provider.String(), record.Title)
					if record.WorkspaceRoot != "" {
						fmt.Fprintf(&out, " (%s)", shortPath(record.WorkspaceRoot))
					}
					out.WriteString("\n")
				}
				return sink.Text(out.String())
			}
			for _, record := range records {
				fmt.Fprintf(
					&out,
					"%s\t%s\t%s\t%s\t%s\n",
					record.ID,
					record.Provider.String(),
					record.NativeID,
					shortPath(record.WorkspaceRoot),
					record.Title,
				)
			}
			return sink.Text(out.String())
		},
	}
}

// getConversationOp prints a conversation transcript as plain text.
func getConversationOp() Operation[getConversationInput] {
	return Operation[getConversationInput]{
		Name:     Name{Canonical: "get_conversation", CLIOverride: "get"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Get plain text from a conversation.",
		Long:     "Print one conversation transcript as plain text. Resolve the conversation by id, native id, title, or artifact path.",
		Examples: []string{"clyde conversation get claude:1a2b3c --last-n 20"},
		Args: []Arg[getConversationInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *getConversationInput, v string) { in.ConversationID = v }),
		},
		Params: []Param[getConversationInput]{
			IntParam("last_n", "Only return the last N messages.", 0,
				func(in *getConversationInput, v int) { in.LastN = v }),
		},
		New: func() getConversationInput { return getConversationInput{ConversationID: "", LastN: 0} },
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
		Run: func(ctx context.Context, in getContextInput, surface Surface, sink ResultSink) error {
			text, err := daemon.GetConversationContext(ctx, in.ConversationID, in.Timestamp, in.MessageIndex, in.Before, in.After)
			if err != nil {
				return logFail(ctx, surface, "context_failed", "get conversation context", err)
			}
			return sink.Text(text)
		},
	}
}

// searchConversationOp searches one conversation and caches the results.
func searchConversationOp() Operation[searchConversationInput] {
	return Operation[searchConversationInput]{
		Name:     Name{Canonical: "search_conversation", CLIOverride: "search"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Search a conversation and return a result_id for follow-up analysis.",
		Long:     "Search one conversation and print matching excerpts together with a result_id. Pass that result_id to analyze to run the analysis model over the same matches.",
		Examples: []string{"clyde conversation search claude:1a2b3c \"auth timeout\" --depth normal"},
		Args: []Arg[searchConversationInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *searchConversationInput, v string) { in.ConversationID = v }),
			PositionalArg("query", "Natural language query.",
				func(in *searchConversationInput, v string) { in.Query = v }),
		},
		Params: []Param[searchConversationInput]{
			StringParam("depth", "quick, normal, deep, or extra-deep.", "quick", false,
				func(in *searchConversationInput, v string) { in.Depth = v }),
		},
		New: func() searchConversationInput {
			return searchConversationInput{ConversationID: "", Query: "", Depth: "quick"}
		},
		Run: func(ctx context.Context, in searchConversationInput, surface Surface, sink ResultSink) error {
			text, err := daemon.SearchConversation(ctx, in.ConversationID, in.Query, in.Depth)
			if err != nil {
				return logFail(ctx, surface, "search_failed", "search conversation", err)
			}
			return sink.Text(text)
		},
	}
}

// analyzeResultsOp runs the local analysis model over cached search results.
func analyzeResultsOp() Operation[analyzeResultsInput] {
	return Operation[analyzeResultsInput]{
		Name:     Name{Canonical: "analyze_results", CLIOverride: "analyze"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Analyze cached results from clyde_search_conversation.",
		Long:     "Run the analysis model over a cached search result set named by result_id, following the instruction in prompt.",
		Examples: []string{"clyde conversation analyze 7f3e2d10 \"summarize the decisions made\""},
		Params:   nil,
		Args: []Arg[analyzeResultsInput]{
			PositionalArg("result_id", "The result_id returned by clyde_search_conversation.",
				func(in *analyzeResultsInput, v string) { in.ResultID = v }),
			PositionalArg("prompt", "Analysis instruction.",
				func(in *analyzeResultsInput, v string) { in.Prompt = v }),
		},
		New: func() analyzeResultsInput { return analyzeResultsInput{ResultID: "", Prompt: ""} },
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
		Long:     "Export one conversation transcript in the chosen format. The terminal writes to --output when set and otherwise prints to stdout; the MCP tool returns the body as text.",
		Examples: []string{"clyde conversation export claude:1a2b3c --format markdown --output transcript.md"},
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
			BoolParam("include_system_prompts", "Include system-injected prompts.", false,
				func(in *exportInput, v bool) { in.Options.IncludeSystemPrompts = v }),
			BoolParam("include_tool_outputs", "Include tool result bodies.", false,
				func(in *exportInput, v bool) { in.Options.IncludeToolOutputs = v }),
			BoolParam("include_raw_json_metadata", "Include JSON metadata fields.", false,
				func(in *exportInput, v bool) { in.Options.IncludeRawJSONMetadata = v }),
			BoolParam("include_thinking", "Include thinking blocks.", true,
				func(in *exportInput, v bool) { in.Options.IncludeThinking = v }),
			BoolParam("include_tool_calls", "Include tool calls.", true,
				func(in *exportInput, v bool) { in.Options.IncludeToolCalls = v }),
			BoolParam("include_chat", "Include chat text.", true,
				func(in *exportInput, v bool) { in.Options.IncludeChat = v }),
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
					IncludeToolCalls:       true,
					IncludeToolOutputs:     false,
					IncludeRawJSONMetadata: false,
				},
			}
		},
		Run: func(ctx context.Context, in exportInput, surface Surface, sink ResultSink) error {
			body, err := daemon.ExportTranscript(ctx, in.ConversationID, in.Options)
			if err != nil {
				return logFail(ctx, surface, "export_failed", "export transcript", err)
			}
			if in.OutputPath != "" {
				return sink.WriteFile(in.OutputPath, body)
			}
			return sink.Bytes(body)
		},
	}
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
