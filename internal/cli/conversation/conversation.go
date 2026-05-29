// Package conversation exposes transcript search and export CLI commands.
package conversation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// NewCommands returns the CLI commands that mirror Clyde's MCP tools.
func NewCommands(f *cli.Factory) []*cobra.Command {
	return []*cobra.Command{
		newListConversationsCmd(f),
		newGetConversationCmd(f),
		newGetContextCmd(f),
		newSearchConversationCmd(f),
		newAnalyzeResultsCmd(f),
		newExportTranscriptCmd(f),
	}
}

func newListConversationsCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "list-conversations",
		Short: "List Claude and Codex conversations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			records, err := conv.DefaultIndex.List(cmd.Context())
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.list_failed", "list conversations", err)
			}
			if len(records) == 0 {
				_, _ = fmt.Fprintln(f.IOStreams.Out, "No conversations found.")
				return nil
			}
			for _, record := range records {
				_, _ = fmt.Fprintf(
					f.IOStreams.Out,
					"%s\t%s\t%s\t%s\t%s\n",
					record.ID,
					record.Provider.String(),
					record.NativeID,
					shortPath(record.WorkspaceRoot),
					record.Title,
				)
			}
			return nil
		},
	}
}

func newGetConversationCmd(f *cli.Factory) *cobra.Command {
	var lastN int
	cmd := &cobra.Command{
		Use:   "get-conversation CONVERSATION_ID",
		Short: "Print a conversation transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			record, err := conv.DefaultIndex.Resolve(cmd.Context(), args[0])
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.resolve_failed", "resolve conversation", err)
			}
			messages, err := conv.LoadMessages(record, false, false)
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.load_failed", "load conversation", err)
			}
			if lastN > 0 && lastN < len(messages) {
				messages = messages[len(messages)-lastN:]
			}
			text := transcript.RenderPlainTextWithOptions(messages, transcript.DefaultShapeOptions())
			if text == "" {
				text = "No conversation messages found.\n"
			}
			_, _ = fmt.Fprint(f.IOStreams.Out, text)
			return nil
		},
	}
	cmd.Flags().IntVar(&lastN, "last-n", 0, "only print the last N messages")
	return cmd
}

func newGetContextCmd(f *cli.Factory) *cobra.Command {
	var timestamp string
	var messageIndex int
	var before int
	var after int
	cmd := &cobra.Command{
		Use:   "get-context CONVERSATION_ID",
		Short: "Print messages around a point in a conversation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			record, err := conv.DefaultIndex.Resolve(cmd.Context(), args[0])
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.resolve_failed", "resolve conversation", err)
			}
			messages, err := conv.LoadMessages(record, false, false)
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.load_failed", "load conversation", err)
			}
			center := messageIndex
			if timestamp != "" {
				center = findNearestMessage(messages, timestamp)
			}
			if center < 0 || center >= len(messages) {
				return fmt.Errorf("provide --timestamp or --message-index inside 0..%d", len(messages)-1)
			}
			start := max(center-before, 0)
			end := min(center+after+1, len(messages))
			window := messages[start:end]
			_, _ = fmt.Fprintf(f.IOStreams.Out, "Messages %d-%d of %d centered on %d:\n\n", start, end-1, len(messages), center)
			_, _ = fmt.Fprint(f.IOStreams.Out, transcript.RenderPlainTextWithOptions(window, transcript.DefaultShapeOptions()))
			return nil
		},
	}
	cmd.Flags().StringVar(&timestamp, "timestamp", "", "timestamp to center on")
	cmd.Flags().IntVar(&messageIndex, "message-index", -1, "message index to center on")
	cmd.Flags().IntVar(&before, "before", 5, "messages before center")
	cmd.Flags().IntVar(&after, "after", 5, "messages after center")
	return cmd
}

func newSearchConversationCmd(f *cli.Factory) *cobra.Command {
	var depth string
	cmd := &cobra.Command{
		Use:   "search-conversation CONVERSATION_ID QUERY",
		Short: "Search a conversation transcript",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := f.Config()
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.config_failed", "load config", err)
			}
			record, err := conv.DefaultIndex.Resolve(cmd.Context(), args[0])
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.resolve_failed", "resolve conversation", err)
			}
			output, err := conv.SearchConversation(cmd.Context(), record, args[1], depth, cfg.Search)
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.search_failed", "search conversation", err)
			}
			_, _ = fmt.Fprint(f.IOStreams.Out, output.Text)
			return nil
		},
	}
	cmd.Flags().StringVar(&depth, "depth", "quick", "search depth: quick, normal, deep, or extra-deep")
	return cmd
}

func newAnalyzeResultsCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "analyze-results RESULT_ID PROMPT",
		Short: "Analyze cached search results",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := f.Config()
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.config_failed", "load config", err)
			}
			text, err := conv.AnalyzeSearchResults(cmd.Context(), args[0], args[1], cfg.Search)
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.analyze_failed", "analyze results", err)
			}
			_, _ = fmt.Fprintln(f.IOStreams.Out, text)
			return nil
		},
	}
}

func newExportTranscriptCmd(f *cli.Factory) *cobra.Command {
	slog.Debug("cli.conversation.export.command_constructed", "concern", "cli.conversation", "component", "cli")
	options := conv.ExportOptions{
		Format:                 conv.ExportFormatMarkdown,
		HistoryStart:           0,
		Whitespace:             conv.WhitespacePreserve,
		IncludeChat:            true,
		IncludeThinking:        true,
		IncludeSystemPrompts:   false,
		IncludeToolCalls:       true,
		IncludeToolOutputs:     false,
		IncludeRawJSONMetadata: false,
	}
	var outputPath string
	cmd := &cobra.Command{
		Use:   "export-transcript CONVERSATION_ID",
		Short: "Export a conversation transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			record, err := conv.DefaultIndex.Resolve(cmd.Context(), args[0])
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.resolve_failed", "resolve conversation", err)
			}
			body, err := conv.Export(record, options)
			if err != nil {
				return commandError(cmd.Context(), "cli.conversation.export_failed", "export transcript", err)
			}
			if outputPath != "" {
				if err := os.WriteFile(outputPath, body, 0o600); err != nil {
					return commandError(cmd.Context(), "cli.conversation.write_export_failed", "write transcript export", err)
				}
				return nil
			}
			_, _ = f.IOStreams.Out.Write(body)
			return nil
		},
	}
	cmd.Flags().Var((*exportFormatValue)(&options.Format), "format", "markdown, html, json, or plain_text")
	cmd.Flags().Var((*whitespaceValue)(&options.Whitespace), "whitespace", "preserve, tidy, compact, or dense")
	cmd.Flags().StringVar(&outputPath, "output", "", "write output to path")
	cmd.Flags().IntVar(&options.HistoryStart, "history-start", 0, "first message index to include")
	cmd.Flags().BoolVar(&options.IncludeSystemPrompts, "include-system-prompts", false, "include system-injected prompts")
	cmd.Flags().BoolVar(&options.IncludeToolOutputs, "include-tool-outputs", false, "include tool result bodies")
	cmd.Flags().BoolVar(&options.IncludeRawJSONMetadata, "include-raw-json-metadata", false, "include JSON metadata fields")
	cmd.Flags().BoolVar(&options.IncludeThinking, "include-thinking", true, "include thinking blocks")
	cmd.Flags().BoolVar(&options.IncludeToolCalls, "include-tool-calls", true, "include tool calls")
	cmd.Flags().BoolVar(&options.IncludeChat, "include-chat", true, "include chat text")
	return cmd
}

func commandError(ctx context.Context, event string, operation string, err error) error {
	slog.WarnContext(ctx, event, "concern", "cli.conversation", "component", "cli", "operation", operation, "err", err)
	return fmt.Errorf("%s: %w", operation, err)
}

type exportFormatValue conv.ExportFormat

func (v *exportFormatValue) String() string {
	return string(*v)
}

func (v *exportFormatValue) Set(raw string) error {
	value := conv.ExportFormat(strings.TrimSpace(raw))
	switch value {
	case conv.ExportFormatMarkdown, conv.ExportFormatHTML, conv.ExportFormatJSON, conv.ExportFormatPlainText:
		*v = exportFormatValue(value)
		return nil
	default:
		return fmt.Errorf("unsupported export format %q", raw)
	}
}

func (v *exportFormatValue) Type() string {
	return "format"
}

type whitespaceValue conv.WhitespaceMode

func (v *whitespaceValue) String() string {
	return string(*v)
}

func (v *whitespaceValue) Set(raw string) error {
	value := conv.WhitespaceMode(strings.TrimSpace(raw))
	switch value {
	case conv.WhitespacePreserve, conv.WhitespaceTidy, conv.WhitespaceCompact, conv.WhitespaceDense:
		*v = whitespaceValue(value)
		return nil
	default:
		return fmt.Errorf("unsupported whitespace mode %q", raw)
	}
}

func (v *whitespaceValue) Type() string {
	return "whitespace"
}

func findNearestMessage(messages []transcript.Message, rawTimestamp string) int {
	var target time.Time
	layouts := []string{
		"2006-01-02 15:04",
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
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
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~/" + path[len(home)+1:]
	}
	return path
}
