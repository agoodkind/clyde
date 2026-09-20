package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/exportargs"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/tokencount"
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

var whitespaceValues = []string{
	string(conv.WhitespacePreserve),
	string(conv.WhitespaceTidy),
	string(conv.WhitespaceCompact),
	string(conv.WhitespaceDense),
}

// exportBind writes one declared export argument into exportInput. Exactly one
// field is non-nil, matching the declaration's kind.
type exportBind struct {
	str      func(in *exportInput, v string)
	integer  func(in *exportInput, v int)
	boolean  func(in *exportInput, v bool)
	strSlice func(in *exportInput, v []string)
}

func stringBind(set func(in *exportInput, v string)) exportBind {
	return exportBind{str: set, integer: nil, boolean: nil, strSlice: nil}
}

func intBind(set func(in *exportInput, v int)) exportBind {
	return exportBind{str: nil, integer: set, boolean: nil, strSlice: nil}
}

func boolBind(set func(in *exportInput, v bool)) exportBind {
	return exportBind{str: nil, integer: nil, boolean: set, strSlice: nil}
}

func strSliceBind(set func(in *exportInput, v []string)) exportBind {
	return exportBind{str: nil, integer: nil, boolean: nil, strSlice: set}
}

// whitespaceShortcutBind records the selection as well as the mode. Prepare
// reads the recorded selections to reject two conflicting whitespace flags.
func whitespaceShortcutBind(mode conv.WhitespaceMode) exportBind {
	return boolBind(func(in *exportInput, v bool) {
		if v {
			in.Options.Whitespace = mode
			recordWhitespaceSelection(in, mode)
		}
	})
}

// contentShortcutBind appends the selector to the same list --only appends to.
// conv.ResolveContentKinds then resolves both through one call.
func contentShortcutBind(selector string) exportBind {
	return boolBind(func(in *exportInput, v bool) {
		if v {
			in.Kinds = append(in.Kinds, selector)
		}
	})
}

// exportBinds returns one closure per declared export argument, keyed by
// canonical name. A declaration with no entry here panics when registerFlag
// applies the flag. TestExportParamsMatchDeclarations fails before that.
func exportBinds() map[string]exportBind {
	binds := map[string]exportBind{
		"format": stringBind(func(in *exportInput, v string) { in.Options.Format = conv.ExportFormat(v) }),
		"whitespace": stringBind(func(in *exportInput, v string) {
			mode := conv.WhitespaceMode(v)
			in.Options.Whitespace = mode
			recordWhitespaceSelection(in, mode)
		}),
		"history_start":       intBind(func(in *exportInput, v int) { in.Options.HistoryStart = v }),
		"include_compactions": stringBind(func(in *exportInput, v string) { in.Options.Compaction.IncludeSelector = v }),
		"full_history":        boolBind(func(in *exportInput, v bool) { in.Options.Compaction.FullHistory = v }),
		"last_n":              intBind(func(in *exportInput, v int) { in.Options.LastN = v }),
		"max_lines":           intBind(func(in *exportInput, v int) { in.Options.MaxLines = v }),
		"max_tokens":          stringBind(func(in *exportInput, v string) { in.Options.MaxTokens = v }),
		"token_model":         stringBind(func(in *exportInput, v string) { in.Options.TokenModel = v }),
		"only":                strSliceBind(func(in *exportInput, v []string) { in.Kinds = append(in.Kinds, v...) }),
	}
	for _, declaration := range exportargs.Declarations() {
		if _, ok := binds[declaration.Canonical]; ok {
			continue
		}
		if slices.Contains(whitespaceValues, declaration.Canonical) {
			binds[declaration.Canonical] = whitespaceShortcutBind(conv.WhitespaceMode(declaration.Canonical))
			continue
		}
		if slices.Contains(conv.ContentKindSelectorValues(), declaration.Canonical) {
			binds[declaration.Canonical] = contentShortcutBind(declaration.Canonical)
		}
	}
	return binds
}

func renderExportParam(declaration exportargs.Declaration, bind exportBind) Param[exportInput] {
	var param Param[exportInput]
	switch declaration.Kind {
	case exportargs.KindString:
		param = StringParam(declaration.Canonical, declaration.Description, declaration.DefaultStr, declaration.Required, bind.str)
	case exportargs.KindInt:
		param = IntParam(declaration.Canonical, declaration.Description, declaration.DefaultInt, bind.integer)
	case exportargs.KindBool:
		param = BoolParam(declaration.Canonical, declaration.Description, declaration.DefaultBool, bind.boolean)
	case exportargs.KindEnum:
		param = EnumParam(declaration.Canonical, declaration.Description, declaration.DefaultStr, declaration.Values, bind.str)
	case exportargs.KindEnumList:
		param = EnumListParam(declaration.Canonical, declaration.Description, declaration.Values, declaration.Required, bind.strSlice)
	}
	param.Required = declaration.Required
	param.CLIOnly = declaration.CLIOnly
	return param
}

// exportParams renders internal/conversation/exportargs as terminal and MCP
// parameters, and declares output and stdout itself. Those two select a
// destination file. The compaction path accepts no destination.
func exportParams() []Param[exportInput] {
	outputPathParam := StringParam("output", "Write output to path. MCP requires an absolute path. Use - for terminal stdout.", "", false,
		func(in *exportInput, v string) { in.OutputPath = v })

	stdoutParam := BoolParam("stdout", "Write the export body directly to stdout. Equivalent to --output -.", false,
		func(in *exportInput, v bool) { in.Stdout = v })
	stdoutParam.CLIOnly = true

	binds := exportBinds()
	declarations := exportargs.Declarations()
	params := make([]Param[exportInput], 0, len(declarations)+2)
	for _, declaration := range declarations {
		params = append(params, renderExportParam(declaration, binds[declaration.Canonical]))
	}
	return append(params, outputPathParam, stdoutParam)
}

func exportTranscriptOp() Operation[exportInput, exportPayload] {
	return Operation[exportInput, exportPayload]{
		Name:       Name{Canonical: "export_transcript", CLIOverride: "export"},
		Group:      conversationGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindArtifact,
		Short:      "Export a conversation transcript.",
		Long:       "Export one conversation transcript in the chosen format. Terminal export reads local artifacts and does not require the daemon. MCP export uses the daemon. Name the content kinds with --only or the per-type shortcut flags; export selects nothing by default. By default, export includes compaction segment 0, which is the latest compaction summary through the latest message. Use --include-compactions all or --full-history to export every segment. On the terminal, with no destination and no --copy, export writes the default artifact file; pass --output PATH to write a file, or --stdout or --output - to write the export body to stdout. The global --copy flag copies the body to the clipboard and, on its own, replaces the default file write, so --copy alone copies without writing a file; combine --copy with --output to also write a file. Clipboard copy matches the selected format, is macOS only for now, and errors on other platforms. The MCP tool returns the transcript body unless output names an absolute file path.",
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
			MaxTokens:    "",
			TokenModel:   "",
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
func exportDestinationPath(ctx context.Context, p exportPayload) (string, error) {
	if surfaceFromContext(ctx) == SurfaceMCP {
		if p.OutputPath == "" {
			return "", nil
		}
		if !filepath.IsAbs(p.OutputPath) {
			return "", fmt.Errorf("MCP output path must be absolute")
		}
		return filepath.Clean(p.OutputPath), nil
	}
	if p.OutputPath != "" {
		return absoluteExportPath(ctx, p.OutputPath)
	}
	if p.Stdout || copyRequested(ctx) {
		return "", nil
	}
	return absoluteExportPath(ctx, defaultExportOutputPath(p.ConversationID, p.Options.Format))
}

func absoluteExportPath(ctx context.Context, path string) (string, error) {
	workingDirectory, err := exportWorkingDirectory(ctx)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDirectory, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		slog.WarnContext(ctx, "clispec.export_path_resolve_failed", "concern", "cli.conversation", "component", "clispec", "path", path, "err", err)
		return "", fmt.Errorf("resolve export path %q: %w", path, err)
	}
	return absPath, nil
}

func exportWorkingDirectory(ctx context.Context) (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		slog.WarnContext(ctx, "clispec.export_cli_cwd_failed", "concern", "cli.conversation", "component", "clispec", "err", err)
		return "", fmt.Errorf("get CLI working directory: %w", err)
	}
	return directory, nil
}

func runExportTranscriptResult(ctx context.Context, p exportPayload) (Result, error) {
	var body []byte
	var err error
	if surfaceFromContext(ctx) == SurfaceCLI {
		body, err = daemon.ExportTranscriptLocal(ctx, p.ConversationID, p.Options)
	} else {
		body, err = daemon.ExportTranscript(ctx, p.ConversationID, p.Options)
	}
	if err != nil {
		return nil, logOperationError(ctx, "export transcript", err)
	}
	spec := exportTokenSpec(ctx, p.ConversationID, p.Options.TokenModel)
	path, err := exportDestinationPath(ctx, p)
	if err != nil {
		slog.WarnContext(ctx, "clispec.export_destination_invalid", "concern", "cli.conversation", "component", "clispec", "err", err)
		return nil, fmt.Errorf("select export destination: %w", err)
	}
	text := ""
	if path != "" {
		text = wroteConfirmation(path, body, tokenEstimateFromSpec(spec, body))
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
		Tokens:      spec,
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
					MaxTokens:    "",
					TokenModel:   "",
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
	var body []byte
	var err error
	if surface == SurfaceCLI {
		body, err = daemon.ExportTranscriptLocal(ctx, p.ConversationID, p.Options)
	} else {
		body, err = daemon.ExportTranscript(ctx, p.ConversationID, p.Options)
	}
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
	path, err := exportDestinationPath(ctx, p)
	if err != nil {
		slog.WarnContext(ctx, "clispec.export_destination_invalid", "concern", "cli.conversation", "component", "clispec", "err", err)
		return fmt.Errorf("select export destination: %w", err)
	}
	if err := sink.WriteFile(path, body); err != nil {
		slog.WarnContext(ctx, "cli.conversation.export_write_failed", "concern", "cli.conversation", "component", "cli", "path", path, "err", err)
		return fmt.Errorf("export transcript: write output %s: %w", path, err)
	}
	spec := exportTokenSpec(ctx, p.ConversationID, p.Options.TokenModel)
	if err := sink.Text(wroteConfirmation(path, body, tokenEstimateFromSpec(spec, body))); err != nil {
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

var lookupExportTokenSpec = daemon.ExportTokenSpec

func exportTokenSpec(ctx context.Context, conversationID, tokenModel string) *tokencount.Spec {
	spec, err := lookupExportTokenSpec(ctx, conversationID, tokenModel)
	if err != nil {
		slog.WarnContext(ctx, "clispec.export_token_spec_failed", "concern", "cli.conversation", "component", "clispec", "conversation_id", conversationID, "err", err)
		return nil
	}
	return spec
}

func tokenEstimateFromSpec(spec *tokencount.Spec, body []byte) tokencount.Estimate {
	if spec == nil {
		return tokencount.Estimate{Tokenizer: "", Tokens: 0}
	}
	return spec.Count(string(body))
}
