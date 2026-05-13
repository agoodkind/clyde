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
	claudelifecycle "goodkind.io/clyde/internal/providers/claude/lifecycle"
	"goodkind.io/clyde/internal/session"
	sessionsettings "goodkind.io/clyde/internal/session/settings"
)

func mergeTypeFlag(s *compactengine.Strippers, csv string) error {
	if csv == "" {
		return nil
	}
	for raw := range strings.SplitSeq(csv, ",") {
		raw = strings.TrimSpace(raw)
		switch raw {
		case "":
			continue
		case "all":
			s.SetAll()
		case "tools":
			s.Tools = true
		case "thinking":
			s.Thinking = true
		case "images":
			s.Images = true
		case "chat":
			s.Chat = true
		default:
			return fmt.Errorf("unknown --type entry %q (expected tools|thinking|images|chat|all)", raw)
		}
	}
	return nil
}

func resolveModelLikeTUI(
	store session.Store,
	sess *session.Session,
	fallback string,
) (countModel string, displayModel string, source string) {
	if sess != nil && sess.Metadata.ProviderTranscriptPath() != "" {
		rawModel, _ := claudelifecycle.ExtractRawModelAndLastTime(sess.Metadata.ProviderTranscriptPath())
		rawModel = strings.TrimSpace(rawModel)
		if rawModel != "" {
			return rawModel, claudelifecycle.FormatModelFamily(rawModel), "transcript"
		}
	}
	if store != nil && sess != nil && strings.TrimSpace(sess.Name) != "" {
		settings, err := sessionsettings.Load(store, sess)
		if err == nil && settings != nil && strings.TrimSpace(settings.Model) != "" {
			settingsModel := strings.TrimSpace(settings.Model)
			return settingsModel, settingsModel, "settings"
		}
	}
	return fallback, fallback, "fallback"
}

type compactCommandInput struct {
	Name            string
	Session         *session.Session
	Store           session.Store
	Transcript      string
	Target          int
	Strippers       compactengine.Strippers
	Apply           bool
	Force           bool
	ForceOverTarget bool
	Reserved        int
	Model           string
	ModelDisplay    string
	ModelExplicit   bool
	ShowPasses      bool
	SummarizeMode   string
}

func runCompact(cmd *cobra.Command, f *cli.Factory, args []string) error {
	name := args[0]
	cliCompactLog.Logger().Info("cli.compact.invoked", "session", name)

	if _, err := f.Config(); err != nil {
		cliCompactLog.Logger().Error("cli.compact.config_failed", "session", name, "err", err)
		return err
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
	if !input.Strippers.Any() && input.Target == 0 {
		refresh, _ := cmd.Flags().GetBool("refresh")
		return runMetricsDashboard(cmd, out, input.Session, input.Transcript, refresh)
	}
	return runCompactRouted(cmd, out, input)
}

func prepareCompactCommandInput(f *cli.Factory, name string) (compactCommandInput, error) {
	store, err := f.Store()
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.store_failed", "session", name, "err", err)
		return compactCommandInput{}, err
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
		Name:            name,
		Session:         sess,
		Store:           store,
		Transcript:      path,
		ForceOverTarget: false,
	}, nil
}

func completeCompactCommandInput(cmd *cobra.Command, input compactCommandInput, args []string) (compactCommandInput, error) {
	target, err := parseCompactTarget(cmd, input.Name, args)
	if err != nil {
		return compactCommandInput{}, err
	}
	flags, err := readCompactFlags(cmd, input.Store, input.Session, target)
	if err != nil {
		return compactCommandInput{}, err
	}
	input.Target = target
	input.Strippers = flags.Strippers
	input.Apply = flags.Apply
	input.Force = flags.Force
	input.ForceOverTarget = flags.ForceOverTarget
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
		cliCompactLog.Logger().Error("cli.compact.resolve_failed", "session", name, "err", err)
		return nil, err
	}
	if sess == nil {
		cliCompactLog.Logger().Warn("cli.compact.session_not_found", "session", name)
		return nil, fmt.Errorf("session %q not found", name)
	}
	return sess, nil
}

func validateCompactTranscript(name string, sess *session.Session) (string, error) {
	path := sess.Metadata.ProviderTranscriptPath()
	if path == "" {
		cliCompactLog.Logger().Warn("cli.compact.no_transcript_path", "session", name, "session_id", sess.Metadata.ProviderSessionID())
		return "", fmt.Errorf("session %q has no transcript path", name)
	}
	if _, err := os.Stat(path); err != nil {
		cliCompactLog.Logger().Error("cli.compact.transcript_stat_failed", "session", name, "transcript", path, "err", err)
		return "", fmt.Errorf("transcript not found: %s", path)
	}
	return path, nil
}

func parseCompactTarget(cmd *cobra.Command, name string, args []string) (int, error) {
	targetFlag, _ := cmd.Flags().GetString("target")
	targetRaw := strings.TrimSpace(targetFlag)
	if targetRaw == "" && len(args) == 2 {
		targetRaw = args[1]
	}
	if targetRaw == "" {
		return 0, nil
	}
	target, err := ParseTokenCount(targetRaw)
	if err != nil {
		slog.WarnContext(cmd.Context(), "cli.compact.invalid_target", "session", name, "target_raw", targetRaw, "err", err)
		return 0, fmt.Errorf("invalid target %q: %w", targetRaw, err)
	}
	return target, nil
}

func readCompactFlags(cmd *cobra.Command, store session.Store, sess *session.Session, target int) (compactCommandInput, error) {
	flagTools, _ := cmd.Flags().GetBool("tools")
	flagThinking, _ := cmd.Flags().GetBool("thinking")
	flagImages, _ := cmd.Flags().GetBool("images")
	flagChat, _ := cmd.Flags().GetBool("chat")
	flagAll, _ := cmd.Flags().GetBool("all")
	flagTypes, _ := cmd.Flags().GetString("type")
	apply, _ := cmd.Flags().GetBool("apply")
	force, _ := cmd.Flags().GetBool("force")
	forceOverTarget, _ := cmd.Flags().GetBool("force-over-target")
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
			return compactCommandInput{}, fmt.Errorf("parse summarize mode: %w", modeErr)
		}
		summarizeMode = string(mode)
	}
	if !modelExplicit {
		resolvedModel, resolvedDisplayModel, resolvedSource := resolveModelLikeTUI(store, sess, model)
		model = resolvedModel
		modelDisplay = resolvedDisplayModel
		cliCompactLog.Logger().Info("cli.compact.model_resolved",
			"session", sess.Name,
			"model_count", model,
			"model_display", modelDisplay,
			"source", resolvedSource,
		)
	}

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
		return compactCommandInput{}, err
	}
	if target > 0 && !strippers.Any() {
		strippers.SetAll()
	}
	if strippers.Chat && target == 0 {
		cliCompactLog.Logger().Warn("cli.compact.chat_requires_target", "session", sess.Name)
		return compactCommandInput{}, fmt.Errorf("--chat requires a positive target token count")
	}

	return compactCommandInput{
		Strippers:       strippers,
		Apply:           apply,
		Force:           force,
		ForceOverTarget: forceOverTarget,
		Reserved:        reserved,
		Model:           model,
		ModelDisplay:    modelDisplay,
		ModelExplicit:   modelExplicit,
		ShowPasses:      showPasses,
		SummarizeMode:   summarizeMode,
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
// now owns the transcript load, the planner loop, the /context Prober
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
		SessionName:     input.Session.Name,
		Mode:            mode,
		Target:          input.Target,
		Reserved:        input.Reserved,
		Model:           input.Model,
		ModelExplicit:   input.ModelExplicit,
		Strippers:       input.Strippers,
		Summarize:       input.SummarizeMode == string(compactengine.SummarizeModeOn),
		SummarizeMode:   input.SummarizeMode,
		Force:           input.Force,
		ForceOverTarget: input.ForceOverTarget,
		ShowPasses:      input.ShowPasses && !isTTY,
		IsTTY:           isTTY && !jsonMode,
		TranscriptPath:  input.Transcript,
		JSONMode:        jsonMode,
		CompactRunID:    uuid.NewString(),
	})
	if daemonErr == nil {
		cliCompactLog.Logger().Info("cli.compact.completed_via_daemon", "session", input.Name, "mode", mode.Label())
		return nil
	}
	cliCompactLog.Logger().Error("cli.compact.daemon_path_failed", "session", input.Name, slog.Any("err", daemonErr))
	return daemonErr
}
