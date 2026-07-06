package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"unicode"

	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
)

type exportInput struct {
	ConversationID       string
	Options              conv.ExportOptions
	OutputPath           string
	Stdout               bool
	Kinds                []string
	WhitespaceSelections []conv.WhitespaceMode
}

func (exportInput) isClispecInput() {}

type exportPayload struct {
	ConversationID string
	Options        conv.ExportOptions
	OutputPath     string
	Stdout         bool
}

func (exportPayload) isClispecPrepared() {}

type exportTailInput struct {
	ConversationID string
	LastN          int
}

func (exportTailInput) isClispecInput() {}

type exportTailPayload struct {
	ConversationID string
	Options        conv.ExportOptions
}

func (exportTailPayload) isClispecPrepared() {}

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

func exportParams() []Param[exportInput] {
	outputPathParam := StringParam("output", "Write output to path, or use - for stdout.", "", false,
		func(in *exportInput, v string) { in.OutputPath = v })
	outputPathParam.CLIOnly = true

	stdoutParam := BoolParam("stdout", "Write the export body directly to stdout. Equivalent to --output -.", false,
		func(in *exportInput, v bool) { in.Stdout = v })
	stdoutParam.CLIOnly = true

	onlyParam := EnumListParam("only",
		"Content kinds to export, comma-separated: chat, thinking, tools, tool_calls, tool_outputs, system_prompts, system_messages, raw_json_metadata, plus all.",
		conv.ContentKindSelectorValues(), true,
		func(in *exportInput, v []string) { in.Kinds = append(in.Kinds, v...) })

	whitespaceParam := EnumParam("whitespace", "preserve, tidy, compact, or dense.", "", whitespaceValues,
		func(in *exportInput, v string) {
			mode := conv.WhitespaceMode(v)
			in.Options.Whitespace = mode
			recordWhitespaceSelection(in, mode)
		})

	shortcut := func(canonical, value, description string) Param[exportInput] {
		param := BoolParam(canonical, description, false, func(in *exportInput, v bool) {
			if v {
				in.Kinds = append(in.Kinds, value)
			}
		})
		param.CLIOnly = true
		return param
	}

	whitespaceShortcut := func(mode conv.WhitespaceMode) Param[exportInput] {
		param := BoolParam(string(mode), "Use "+string(mode)+" whitespace.", false, func(in *exportInput, v bool) {
			if v {
				in.Options.Whitespace = mode
				recordWhitespaceSelection(in, mode)
			}
		})
		param.CLIOnly = true
		return param
	}

	return []Param[exportInput]{
		EnumParam("format", "markdown, html, json, or plain_text.", string(conv.ExportFormatMarkdown), exportFormatValues,
			func(in *exportInput, v string) { in.Options.Format = conv.ExportFormat(v) }),
		whitespaceParam,
		whitespaceShortcut(conv.WhitespacePreserve),
		whitespaceShortcut(conv.WhitespaceTidy),
		whitespaceShortcut(conv.WhitespaceDense),
		outputPathParam,
		stdoutParam,
		IntParam("history_start", "First message index to include.", 0,
			func(in *exportInput, v int) { in.Options.HistoryStart = v }),
		StringParam("include_compactions", "Compaction segments to export: 0, 0,1, 0..2, or all. Defaults to 0.", "", false,
			func(in *exportInput, v string) { in.Options.Compaction.IncludeSelector = v }),
		BoolParam("full_history", "Export all compaction segments. Equivalent to --include-compactions all.", false,
			func(in *exportInput, v bool) { in.Options.Compaction.FullHistory = v }),
		IntParam("last_n", "Keep only the last N visible messages after compaction segment selection.", 0,
			func(in *exportInput, v int) { in.Options.LastN = v }),
		IntParam("max_lines", "Keep only the last N rendered lines after whitespace compression. Zero leaves the output uncapped.", 0,
			func(in *exportInput, v int) { in.Options.MaxLines = v }),
		onlyParam,
		shortcut("chat", "chat", "Include conversation chat text."),
		shortcut("thinking", "thinking", "Include assistant thinking blocks."),
		shortcut("tool_calls", "tool_calls", "Include tool calls."),
		shortcut("tool_outputs", "tool_outputs", "Include tool result bodies."),
		shortcut("system_prompts", "system_prompts", "Include system-injected prompts."),
		shortcut("system_messages", "system_messages", "Include provider system transcript records."),
		shortcut("raw_json_metadata", "raw_json_metadata", "Include JSON metadata fields."),
		shortcut("tools", "tools", "Include summary-only tool lines."),
		shortcut("all", "all", "Include every non-tool kind plus tool outputs."),
	}
}

func exportTranscriptOp() Operation[exportInput, exportPayload] {
	return Operation[exportInput, exportPayload]{
		Name:       Name{Canonical: "export_transcript", CLIOverride: "export"},
		Group:      conversationGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindArtifact,
		Short:      "Export a conversation transcript.",
		Long:       "Export one conversation transcript in the chosen format. Name the content kinds with --only or the per-type shortcut flags; export selects nothing by default. By default, export includes compaction segment 0, which is the latest compaction summary through the latest message. Use --include-compactions all or --full-history to export every segment. On the terminal, with no destination and no --copy, export writes the default artifact file; pass --output PATH to write a file, or --stdout or --output - to write the export body to stdout. The global --copy flag copies the body to the clipboard and, on its own, replaces the default file write, so --copy alone copies without writing a file; combine --copy with --output to also write a file. Clipboard copy matches the selected format, is macOS only for now, and errors on other platforms. The MCP tool returns the body as text.",
		Examples: []string{
			"clyde conversation export claude:1a2b3c --only chat,thinking,tool_calls --output transcript.md",
			"clyde conversation export claude:1a2b3c --only chat --include-compactions 0 --stdout",
			"clyde conversation export claude:1a2b3c --only chat --include-compactions 0..2 --last-n 20 --stdout",
			"clyde conversation export claude:1a2b3c --chat --tools --dense --max-lines 3500 --stdout",
			"clyde conversation export claude:1a2b3c --thinking --tools --stdout",
			"clyde conversation export claude:1a2b3c --all --copy",
			"clyde conversation export claude:1a2b3c --all",
		},
		Args: []Arg[exportInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *exportInput, v string) { in.ConversationID = v }),
		},
		Params:         exportParams(),
		New:            newExportInput,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       []renderable{exportTailOp()},
		Prepare: func(in exportInput) (exportPayload, error) {
			whitespace, err := resolveExportWhitespace(in)
			if err != nil {
				return exportPayload{}, err
			}
			in.Options.Whitespace = whitespace
			content, err := conv.ResolveContentKinds(in.Kinds)
			if err != nil {
				slog.Warn("cli.conversation.export_content_invalid", "concern", "cli.conversation", "component", "cli", "err", err)
				return exportPayload{}, fmt.Errorf("select content kinds: %w", err)
			}
			in.Options.Content = content
			compactionOptions, err := conv.NormalizeCompactionExportOptions(
				in.Options.Compaction,
				in.Options.HistoryStart,
				in.Options.LastN,
			)
			if err != nil {
				slog.Warn("cli.conversation.export_compaction_invalid", "concern", "cli.conversation", "component", "cli", "err", err)
				return exportPayload{}, fmt.Errorf("select compaction controls: %w", err)
			}
			in.Options.Compaction = compactionOptions
			if in.Stdout && in.OutputPath != "" && in.OutputPath != "-" {
				return exportPayload{}, fmt.Errorf("select output destination: --stdout cannot be combined with --output %q", in.OutputPath)
			}
			stdout := in.Stdout || in.OutputPath == "-"
			return exportPayload{
				ConversationID: in.ConversationID,
				Options:        in.Options,
				OutputPath:     in.OutputPath,
				Stdout:         stdout,
			}, nil
		},
		Run:       runExportTranscript,
		runResult: runExportTranscriptResult,
	}
}

func newExportInput() exportInput {
	return exportInput{
		ConversationID:       "",
		OutputPath:           "",
		Stdout:               false,
		Kinds:                nil,
		WhitespaceSelections: nil,
		Options: conv.ExportOptions{
			Format:       conv.ExportFormatMarkdown,
			HistoryStart: 0,
			LastN:        0,
			MaxLines:     0,
			Whitespace:   conv.WhitespacePreserve,
			Content:      conv.NewContentKindSet(),
			Compaction: conv.CompactionExportOptions{
				IncludeSelector: "",
				FullHistory:     false,
			},
		},
	}
}

// exportDestinationPath returns the file path export should write to, or ""
// when export should not write a file. An explicit --output always wins.
// With no explicit path, export writes the implicit default file, unless the
// body is already going to stdout (--stdout) or the clipboard (--copy);
// either of those replaces the implicit file, so --copy alone copies without
// writing a file.
func exportDestinationPath(ctx context.Context, p exportPayload) string {
	if p.OutputPath != "" {
		return p.OutputPath
	}
	if p.Stdout || copyRequested(ctx) {
		return ""
	}
	return defaultExportOutputPath(p.ConversationID, p.Options.Format)
}

func runExportTranscriptResult(ctx context.Context, p exportPayload) (Result, error) {
	body, err := daemon.ExportTranscript(ctx, p.ConversationID, p.Options)
	if err != nil {
		return nil, logOperationError(ctx, "export transcript", err)
	}
	path := exportDestinationPath(ctx, p)
	text := ""
	if path != "" {
		text = "wrote: " + path + "\n"
	}
	return artifactResult{
		Payload: exportTranscriptOutput{
			ConversationID: p.ConversationID,
			Format:         string(p.Options.Format),
			Path:           path,
			Bytes:          len(body),
			Pipe:           p.Stdout,
		},
		Body:        body,
		DefaultPath: path,
		Pipe:        p.Stdout,
		Text:        text,
		InlineText:  string(body),
	}, nil
}

func exportTailOp() Operation[exportTailInput, exportTailPayload] {
	return Operation[exportTailInput, exportTailPayload]{
		Name:       Name{Canonical: "export_tail", CLIOverride: "tail"},
		Group:      nil,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: 0,
		Short:      "Export the latest conversation messages.",
		Long:       "Export the latest visible messages from compaction segment 0 as dense Markdown with chat text and tool summaries.",
		Examples: []string{
			"clyde conversation export tail claude:1a2b3c --last-n 20",
		},
		Args: []Arg[exportTailInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *exportTailInput, v string) { in.ConversationID = v }),
		},
		Params: []Param[exportTailInput]{
			IntParam("last_n", "Visible message count to keep.", 0,
				func(in *exportTailInput, v int) { in.LastN = v }),
		},
		New: func() exportTailInput {
			return exportTailInput{ConversationID: "", LastN: 0}
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       nil,
		Prepare: func(in exportTailInput) (exportTailPayload, error) {
			if in.LastN <= 0 {
				return exportTailPayload{}, fmt.Errorf("--last-n must be greater than 0")
			}
			return exportTailPayload{
				ConversationID: in.ConversationID,
				Options: conv.ExportOptions{
					Format:       conv.ExportFormatMarkdown,
					HistoryStart: 0,
					LastN:        in.LastN,
					MaxLines:     0,
					Whitespace:   conv.WhitespaceDense,
					Content:      conv.NewContentKindSet(conv.ContentKindChat, conv.ContentKindToolSummaries),
					Compaction: conv.CompactionExportOptions{
						IncludeSelector: "0",
						FullHistory:     false,
					},
				},
			}, nil
		},
		Run: func(ctx context.Context, p exportTailPayload, surface Surface, sink ResultSink) error {
			payload := exportPayload{
				ConversationID: p.ConversationID,
				Options:        p.Options,
				OutputPath:     "",
				Stdout:         true,
			}
			return runExportTranscript(ctx, payload, surface, sink)
		},
		runResult: nil,
	}
}

func runExportTranscript(
	ctx context.Context,
	p exportPayload,
	surface Surface,
	sink ResultSink,
) error {
	body, err := daemon.ExportTranscript(ctx, p.ConversationID, p.Options)
	if err != nil {
		return logFail(ctx, surface, "export_failed", "export transcript", err)
	}
	if surface == SurfaceCLI {
		return writeCLIExportBody(ctx, p, body, sink)
	}
	if err := sink.Bytes(body); err != nil {
		slog.WarnContext(ctx, "mcp.conversation.export_write_failed", "concern", "mcp.server.context", "component", "mcpserver", "err", err)
		return fmt.Errorf("export transcript: write MCP body: %w", err)
	}
	return nil
}

func writeCLIExportBody(
	ctx context.Context,
	p exportPayload,
	body []byte,
	sink ResultSink,
) error {
	if p.Stdout {
		if err := sink.RawBytes(body); err != nil {
			slog.WarnContext(ctx, "cli.conversation.export_stdout_write_failed", "concern", "cli.conversation", "component", "cli", "err", err)
			return fmt.Errorf("export transcript: write stdout: %w", err)
		}
		return nil
	}
	return writeCLIExportFile(ctx, p, body, sink)
}

func writeCLIExportFile(
	ctx context.Context,
	p exportPayload,
	body []byte,
	sink ResultSink,
) error {
	path := p.OutputPath
	if path == "" {
		path = defaultExportOutputPath(p.ConversationID, p.Options.Format)
	}
	if err := sink.WriteFile(path, body); err != nil {
		slog.WarnContext(ctx, "cli.conversation.export_write_failed", "concern", "cli.conversation", "component", "cli", "path", path, "err", err)
		return fmt.Errorf("export transcript: write output %s: %w", path, err)
	}
	if err := sink.Text("wrote: " + path + "\n"); err != nil {
		return fmt.Errorf("export transcript: write confirmation: %w", err)
	}
	return nil
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
