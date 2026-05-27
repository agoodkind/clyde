package compact

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	compactengine "goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/session"
)

type stripperTypeToken string

const (
	stripperTypeTokenAll      stripperTypeToken = "all"
	stripperTypeTokenTools    stripperTypeToken = "tools"
	stripperTypeTokenThinking stripperTypeToken = "thinking"
	stripperTypeTokenImages   stripperTypeToken = "images"
	stripperTypeTokenChat     stripperTypeToken = "chat"
)

func mergeTypeFlag(s *compactengine.Strippers, csv string) error {
	if csv == "" {
		return nil
	}
	for raw := range strings.SplitSeq(csv, ",") {
		raw = strings.TrimSpace(raw)
		switch stripperTypeToken(raw) {
		case "":
			continue
		case stripperTypeTokenAll:
			s.SetAll()
		case stripperTypeTokenTools:
			s.Tools = true
		case stripperTypeTokenThinking:
			s.Thinking = true
		case stripperTypeTokenImages:
			s.Images = true
		case stripperTypeTokenChat:
			s.Chat = true
		default:
			return fmt.Errorf("unknown --type entry %q (expected tools|thinking|images|chat|all)", raw)
		}
	}
	return nil
}

type compactCommandInput struct {
	Name          string
	Session       *session.Session
	Store         session.Store
	Transcript    string
	Target        int
	Strippers     compactengine.Strippers
	Apply         bool
	Force         bool
	Reserved      int
	Model         string
	ModelDisplay  string
	ModelExplicit bool
	ShowPasses    bool
	SummarizeMode string
}

func runCompact(cmd *cobra.Command, f *cli.Factory, args []string) error {
	name := args[0]
	cliCompactLog.Logger().Info("cli.compact.invoked", "session", name)

	if _, err := f.Config(); err != nil {
		slog.ErrorContext(cmd.Context(), "cli.compact.config_failed", "session", name, "err", err)
		return fmt.Errorf("load config: %w", err)
	}

	input, err := prepareCompactCommandInput(f, name)
	if err != nil {
		return err
	}
	out := f.IOStreams.Out

	if handled, err := runCompactMaintenanceAction(cmd, out, input); handled || err != nil {
		return err
	}
	input, err = completeCompactCommandInput(cmd, input, args)
	if err != nil {
		return err
	}
	return runCompactRouted(cmd, out, input)
}

func prepareCompactCommandInput(f *cli.Factory, name string) (compactCommandInput, error) {
	store, err := f.Store()
	if err != nil {
		slog.Error("cli.compact.store_failed", "session", name, "err", err)
		return compactCommandInput{}, fmt.Errorf("open session store: %w", err)
	}
	sess, err := resolveCompactSession(store, name)
	if err != nil {
		return compactCommandInput{}, err
	}
	path, err := validateCompactTranscript(name, sess)
	if err != nil {
		return compactCommandInput{}, err
	}
	return compactCommandInput{
		Name:       name,
		Session:    sess,
		Store:      store,
		Transcript: path,
		Target:     0,
		Strippers: compactengine.Strippers{
			Thinking: false,
			Images:   false,
			Tools:    false,
			Chat:     false,
		},
		Apply:         false,
		Force:         false,
		Reserved:      0,
		Model:         "",
		ModelDisplay:  "",
		ModelExplicit: false,
		ShowPasses:    false,
		SummarizeMode: "",
	}, nil
}

func completeCompactCommandInput(cmd *cobra.Command, input compactCommandInput, args []string) (compactCommandInput, error) {
	target, err := parseCompactTarget(cmd, input.Name, args)
	if err != nil {
		return compactCommandInput{}, err
	}
	flags, err := readCompactFlags(cmd, input.Session, target)
	if err != nil {
		return compactCommandInput{}, err
	}
	input.Target = target
	input.Strippers = flags.Strippers
	input.Apply = flags.Apply
	input.Force = flags.Force
	input.Reserved = flags.Reserved
	input.Model = flags.Model
	input.ModelDisplay = flags.ModelDisplay
	input.ModelExplicit = flags.ModelExplicit
	input.ShowPasses = flags.ShowPasses
	input.SummarizeMode = flags.SummarizeMode
	return input, nil
}

func resolveCompactSession(store session.Store, name string) (*session.Session, error) {
	sess, err := store.Resolve(name)
	if err != nil {
		slog.Error("cli.compact.resolve_failed", "session", name, "err", err)
		return nil, fmt.Errorf("resolve compact session %q: %w", name, err)
	}
	if sess == nil {
		cliCompactLog.Logger().Warn("cli.compact.session_not_found", "session", name)
		return nil, fmt.Errorf("session %q not found", name)
	}
	return sess, nil
}

func validateCompactTranscript(name string, sess *session.Session) (string, error) {
	if sess.Metadata.ProviderTranscriptPath() == "" {
		cliCompactLog.Logger().Warn("cli.compact.no_transcript_path", "session", name, "session_id", sess.Metadata.ProviderSessionID())
		return "", fmt.Errorf("session %q has no transcript path", name)
	}
	path := session.EffectiveTranscriptPath(sess)
	if _, err := os.Stat(path); err != nil {
		cliCompactLog.Logger().Error("cli.compact.transcript_stat_failed", "session", name, "transcript", path, "err", err)
		return "", fmt.Errorf("transcript not found: %s", path)
	}
	return path, nil
}

func parseCompactTarget(cmd *cobra.Command, name string, args []string) (int, error) {
	targetFlag, _ := cmd.Flags().GetString("target")
	targetRaw := strings.TrimSpace(targetFlag)
	if targetRaw == "" {
		if secondArg, ok := compactSecondArg(args); ok {
			targetRaw = secondArg
		}
	}
	if targetRaw == "" {
		slog.WarnContext(cmd.Context(), "cli.compact.missing_target", "session", name)
		return 0, fmt.Errorf("target token count is required and must be greater than zero")
	}
	target, err := ParseTokenCount(targetRaw)
	if err != nil {
		slog.WarnContext(cmd.Context(), "cli.compact.invalid_target", "session", name, "target_raw", targetRaw, "err", err)
		return 0, fmt.Errorf("invalid target %q: %w", targetRaw, err)
	}
	if target <= 0 {
		slog.WarnContext(cmd.Context(), "cli.compact.non_positive_target", "session", name, "target_raw", targetRaw, "target", target)
		return 0, fmt.Errorf("target token count must be greater than zero")
	}
	return target, nil
}

func compactSecondArg(args []string) (string, bool) {
	for index, arg := range args {
		if index == 1 {
			return arg, true
		}
	}
	return "", false
}

func readCompactFlags(cmd *cobra.Command, sess *session.Session, target int) (compactCommandInput, error) {
	flagTools, _ := cmd.Flags().GetBool("tools")
	flagThinking, _ := cmd.Flags().GetBool("thinking")
	flagImages, _ := cmd.Flags().GetBool("images")
	flagChat, _ := cmd.Flags().GetBool("chat")
	flagAll, _ := cmd.Flags().GetBool("all")
	flagTypes, _ := cmd.Flags().GetString("type")
	apply, _ := cmd.Flags().GetBool("apply")
	force, _ := cmd.Flags().GetBool("force")
	reserved, _ := cmd.Flags().GetInt("reserved")
	model, _ := cmd.Flags().GetString("model")
	modelDisplay := model
	modelExplicit := cmd.Flags().Changed("model")
	showPasses, _ := cmd.Flags().GetBool("show-passes")
	summarizeMode := "auto"
	if cmd.Flags().Changed("summarize") {
		summarize, _ := cmd.Flags().GetBool("summarize")
		if summarize {
			summarizeMode = "on"
		} else {
			summarizeMode = "off"
		}
	}
	if rawMode, _ := cmd.Flags().GetString("summarize-mode"); cmd.Flags().Changed("summarize-mode") || strings.TrimSpace(rawMode) != "auto" {
		mode, modeErr := compactengine.NormalizeSummarizeMode(rawMode)
		if modeErr != nil {
			slog.WarnContext(cmd.Context(), "cli.compact.summarize_mode_invalid", "session", sess.Name, "summarize_mode", rawMode, "err", modeErr)
			return compactCommandInput{}, fmt.Errorf("parse summarize mode: %w", modeErr)
		}
		summarizeMode = string(mode)
	}
	// No CLI-side model resolution: when --model is unset the daemon
	// resolves modelForCount/modelForRender from session settings via
	// compactengine.ResolveTokenizerModelForRequest and the probe
	// resolves the live model claude-side from --settings.
	strippers := compactengine.Strippers{
		Tools:    flagTools,
		Thinking: flagThinking,
		Images:   flagImages,
		Chat:     flagChat,
	}
	if flagAll {
		strippers.SetAll()
	}
	if err := mergeTypeFlag(&strippers, flagTypes); err != nil {
		cliCompactLog.Logger().Warn("cli.compact.type_flag_invalid", "session", sess.Name, "err", err)
		return compactCommandInput{}, fmt.Errorf("parse compact type flag: %w", err)
	}
	if target > 0 && !strippers.Any() {
		strippers.SetAll()
	}
	if strippers.Chat && target == 0 {
		cliCompactLog.Logger().Warn("cli.compact.chat_requires_target", "session", sess.Name)
		return compactCommandInput{}, fmt.Errorf("--chat requires a positive target token count")
	}

	return compactCommandInput{
		Name:          "",
		Session:       nil,
		Store:         nil,
		Transcript:    "",
		Target:        0,
		Strippers:     strippers,
		Apply:         apply,
		Force:         force,
		Reserved:      reserved,
		Model:         model,
		ModelDisplay:  modelDisplay,
		ModelExplicit: modelExplicit,
		ShowPasses:    showPasses,
		SummarizeMode: summarizeMode,
	}, nil
}

func runCompactMaintenanceAction(cmd *cobra.Command, out io.Writer, input compactCommandInput) (bool, error) {
	if listBackups, _ := cmd.Flags().GetBool("list-backups"); listBackups {
		return true, runListBackups(cmd, out, input.Session)
	}
	if undo, _ := cmd.Flags().GetBool("undo"); undo {
		return true, runUndo(out, input.Session, input.Transcript)
	}
	if calibrationTarget, _ := cmd.Flags().GetInt("calibrate"); calibrationTarget > 0 {
		model, _ := cmd.Flags().GetString("model")
		return true, runCalibrate(cmd.Context(), out, input.Session, calibrationTarget, model)
	}
	if autoCalibrate, _ := cmd.Flags().GetBool("auto-calibrate"); autoCalibrate {
		model, _ := cmd.Flags().GetString("model")
		return true, runAutoCalibrate(cmd.Context(), out, input.Session, model)
	}
	return false, nil
}

func compactMode(apply bool) Mode {
	if apply {
		return ModeApply
	}
	return ModePreview
}

// runCompactRouted forwards every preview or apply that has a target,
// strippers, or both through the daemon CompactPreview / CompactApply
// stream. The local in-process planner used to live here; the daemon
// now owns the transcript load, the planner loop, the context counter
// projections, summarization, and (in apply mode) the on-disk mutation.
func runCompactRouted(cmd *cobra.Command, out io.Writer, input compactCommandInput) error {
	mode := compactMode(input.Apply)
	isTTY := isTerminal(out)

	enc, encErr := output.From(cmd, out)
	if encErr != nil {
		slog.WarnContext(cmd.Context(), "cli.compact.run.encoder_failed",
			"component", "cli",
			"subcomponent", "compact",
			"session", input.Name,
			"err", encErr.Error(),
		)
		return fmt.Errorf("resolve output encoder: %w", encErr)
	}
	jsonMode := enc.Format == output.FormatJSON

	if input.Target > 0 && !jsonMode {
		_, _ = fmt.Fprintf(out, "starting compact %s for %s; gathering startup stats...\n",
			strings.ToLower(mode.Label()), input.Session.Name)
	}

	daemonErr := runCompactViaDaemon(cmd.Context(), out, compactDaemonRunInput{
		SessionName:    input.Session.Name,
		Mode:           mode,
		Target:         input.Target,
		Reserved:       input.Reserved,
		Model:          input.Model,
		ModelExplicit:  input.ModelExplicit,
		Strippers:      input.Strippers,
		Summarize:      input.SummarizeMode == string(compactengine.SummarizeModeOn),
		SummarizeMode:  input.SummarizeMode,
		Force:          input.Force,
		ShowPasses:     input.ShowPasses && !isTTY,
		IsTTY:          isTTY && !jsonMode,
		TranscriptPath: input.Transcript,
		JSONMode:       jsonMode,
		CompactRunID:   uuid.NewString(),
	})
	if daemonErr == nil {
		cliCompactLog.Logger().Info("cli.compact.completed_via_daemon", "session", input.Name, "mode", mode.Label())
		return nil
	}
	cliCompactLog.Logger().Error("cli.compact.daemon_path_failed", "session", input.Name, "err", daemonErr)
	return fmt.Errorf("run compact via daemon: %w", daemonErr)
}
