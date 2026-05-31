package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/homedir"
	"goodkind.io/clyde/internal/transcript"
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
	Use:   "conversation",
	Short: "Inspect Claude and Codex conversations",
	Long:  "",
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
		Long:     "",
		Args:     nil,
		Params:   nil,
		New:      func() listConversationsInput { return listConversationsInput{} },
		Run: func(ctx context.Context, _ listConversationsInput, surface Surface, sink ResultSink) error {
			records, err := conv.DefaultIndex.List(ctx)
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
		Long:     "",
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
			record, err := resolveConversation(ctx, surface, in.ConversationID)
			if err != nil {
				return err
			}
			messages, err := conv.LoadMessages(record, false, false)
			if err != nil {
				return logFail(ctx, surface, "load_failed", "load conversation", err)
			}
			if in.LastN > 0 && in.LastN < len(messages) {
				messages = messages[len(messages)-in.LastN:]
			}
			text := transcript.RenderPlainTextWithOptions(messages, transcript.DefaultShapeOptions())
			if text == "" {
				return sink.Text("No conversation messages found.")
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
		Long:     "",
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
			record, err := resolveConversation(ctx, surface, in.ConversationID)
			if err != nil {
				return err
			}
			messages, err := conv.LoadMessages(record, false, false)
			if err != nil {
				return logFail(ctx, surface, "load_failed", "load conversation", err)
			}
			if len(messages) == 0 {
				return sink.Text("No conversation messages found.")
			}
			center := -1
			if in.Timestamp != "" {
				center = findNearestMessage(messages, in.Timestamp)
			}
			if center < 0 && in.MessageIndex >= 0 && in.MessageIndex < len(messages) {
				center = in.MessageIndex
			}
			if center < 0 {
				return sink.Text("Provide timestamp or message_index to center on.")
			}
			start := max(center-in.Before, 0)
			end := min(center+in.After+1, len(messages))
			text := fmt.Sprintf(
				"Messages %d-%d of %d centered on %d:\n\n%s",
				start,
				end-1,
				len(messages),
				center,
				transcript.RenderPlainTextWithOptions(messages[start:end], transcript.DefaultShapeOptions()),
			)
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
		Long:     "",
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
			if in.Query == "" {
				return sink.Text("query is required")
			}
			cfg, err := config.LoadGlobalOrDefault()
			if err != nil {
				return logFail(ctx, surface, "config_failed", "load config", err)
			}
			record, err := resolveConversation(ctx, surface, in.ConversationID)
			if err != nil {
				return err
			}
			output, err := conv.SearchConversation(ctx, record, in.Query, in.Depth, cfg.Search)
			if err != nil {
				return logFail(ctx, surface, "search_failed", "search conversation", err)
			}
			return sink.Text(output.Text)
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
		Long:     "",
		Params:   nil,
		Args: []Arg[analyzeResultsInput]{
			PositionalArg("result_id", "The result_id returned by clyde_search_conversation.",
				func(in *analyzeResultsInput, v string) { in.ResultID = v }),
			PositionalArg("prompt", "Analysis instruction.",
				func(in *analyzeResultsInput, v string) { in.Prompt = v }),
		},
		New: func() analyzeResultsInput { return analyzeResultsInput{ResultID: "", Prompt: ""} },
		Run: func(ctx context.Context, in analyzeResultsInput, surface Surface, sink ResultSink) error {
			if in.ResultID == "" || in.Prompt == "" {
				return sink.Text("result_id and prompt are required")
			}
			cfg, err := config.LoadGlobalOrDefault()
			if err != nil {
				return logFail(ctx, surface, "config_failed", "load config", err)
			}
			text, err := conv.AnalyzeSearchResults(ctx, in.ResultID, in.Prompt, cfg.Search)
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
		Long:     "",
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
			record, err := resolveConversation(ctx, surface, in.ConversationID)
			if err != nil {
				return err
			}
			body, err := conv.Export(record, in.Options)
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

// resolveConversation looks up one conversation by selector and logs a
// surface-correct failure event when the lookup fails.
func resolveConversation(ctx context.Context, surface Surface, selector string) (conv.Record, error) {
	var zero conv.Record
	if selector == "" {
		return zero, fmt.Errorf("conversation_id is required")
	}
	record, err := conv.DefaultIndex.Resolve(ctx, selector)
	if err != nil {
		return zero, logFail(ctx, surface, "resolve_failed", "resolve conversation", err)
	}
	return record, nil
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

func findNearestMessage(messages []transcript.Message, rawTimestamp string) int {
	var target time.Time
	for _, layout := range []string{
		"2006-01-02 15:04",
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, rawTimestamp)
		if err == nil {
			target = parsed
			break
		}
	}
	if target.IsZero() {
		return -1
	}
	best := -1
	bestDiff := time.Duration(1<<63 - 1)
	for i, message := range messages {
		diff := message.Timestamp.Sub(target)
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff = diff
			best = i
		}
	}
	return best
}

func shortPath(path string) string {
	return homedir.Contract(path)
}
