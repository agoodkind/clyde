package compact

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	compactengine "goodkind.io/clyde/internal/compact"
	contextusage "goodkind.io/clyde/internal/providers/claude/contextusage"
	claudelifecycle "goodkind.io/clyde/internal/providers/claude/lifecycle"
	"goodkind.io/clyde/internal/session"
	sessionsettings "goodkind.io/clyde/internal/session/settings"
)

// layerCounter adapts contextusage.Layer to the planner's Counter
// interface. Every count_tokens call from the target loop goes
// through the unified layer so future backend swaps (local
// tokenizer, cached responses, etc.) need to change only one place.
type layerCounter struct {
	layer contextusage.Layer
	model string
}

// CountSyntheticUser forwards to Layer.Count with the layer's default
// model. The planner never overrides the model per call, so
// CountOptions stays empty.
func (c *layerCounter) CountSyntheticUser(ctx context.Context, contentArray []compactengine.OutputBlock) (int, error) {
	return c.layer.Count(ctx, contentArray, contextusage.CountOptions{Model: c.model})
}

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
	if input.Target > 0 {
		return runTargetCompactViaDaemon(cmd, out, input)
	}
	return runLocalCompact(cmd, out, input)
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
		Name:       name,
		Session:    sess,
		Store:      store,
		Transcript: path,
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
		return true, runCalibrate(out, input.Session, calibrationTarget, model)
	}
	if autoCalibrate, _ := cmd.Flags().GetBool("auto-calibrate"); autoCalibrate {
		model, _ := cmd.Flags().GetString("model")
		return true, runAutoCalibrate(cmd.Context(), out, input.Session, model)
	}
	return false, nil
}

func runTargetCompactViaDaemon(cmd *cobra.Command, out io.Writer, input compactCommandInput) error {
	mode := compactMode(input.Apply)
	isTTY := isTerminal(out)
	_, _ = fmt.Fprintf(out, "starting compact %s for %s; gathering startup stats...\n",
		strings.ToLower(mode.Label()), input.Session.Name)
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
		IsTTY:          isTTY,
		TranscriptPath: input.Transcript,
	})
	if daemonErr == nil {
		cliCompactLog.Logger().Info("cli.compact.completed_via_daemon", "session", input.Name, "mode", mode.Label())
		return nil
	}
	cliCompactLog.Logger().Error("cli.compact.daemon_path_failed", "session", input.Name, slog.Any("err", daemonErr))
	return daemonErr
}

func compactMode(apply bool) Mode {
	if apply {
		return ModeApply
	}
	return ModePreview
}

func runLocalCompact(cmd *cobra.Command, out io.Writer, input compactCommandInput) error {
	slice, err := compactengine.LoadSlice(input.Transcript)
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.load_slice_failed", "session", input.Name, "transcript", input.Transcript, "err", err)
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
	defer cancel()
	mode := compactMode(input.Apply)
	staticOverhead, err := loadStaticOverhead(cmd, out, input)
	if err != nil {
		return err
	}
	upfrontStats := renderCompactUpfrontStats(ctx, out, input, slice, mode, staticOverhead)
	counter, err := newCompactCounter(input)
	if err != nil {
		return err
	}

	enc, encErr := output.From(cmd, out)
	if encErr != nil {
		slog.Warn("cli.compact.run.encoder_failed",
			"component", "cli",
			"subcomponent", "compact",
			"session", input.Name,
			"err", encErr,
		)
		return fmt.Errorf("resolve output encoder: %w", encErr)
	}
	jsonMode := enc.Format == output.FormatJSON

	planResult, isTTY, progress, err := runCompactPlan(ctx, out, input, slice, mode, staticOverhead, counter, upfrontStats, jsonMode)
	if err != nil {
		return err
	}
	if jsonMode {
		emitErr := emitStreamEvent(out, ResultEvent{
			Event:        "result",
			HitTarget:    planResult.HitTarget,
			BaselineTail: planResult.BaselineTail,
			FinalTail:    planResult.FinalTail,
			Target:       input.Target,
			CompactRunID: "",
		})
		if emitErr != nil {
			return emitErr
		}
	} else {
		renderCompactResult(out, input, slice, planResult, progress, staticOverhead, isTTY)
	}
	if !input.Apply {
		cliCompactLog.Logger().Info("cli.compact.preview.completed", "session", input.Name, "applied", false)
		return nil
	}
	maybeSummarizeCompact(ctx, out, input, slice, planResult)
	if err := runApply(out, input.Session, slice, input.Strippers, input.Target, planResult, input.Force); err != nil {
		return err
	}
	if !isTTY {
		RenderFinalApply(out, planResult, input.Target, staticOverhead, input.Reserved, input.Transcript)
	}
	return nil
}

func loadStaticOverhead(cmd *cobra.Command, out io.Writer, input compactCommandInput) (int, error) {
	if input.Target == 0 {
		return 0, nil
	}
	cal, ok, err := compactengine.LoadCalibration(input.Session.Metadata.ProviderSessionID())
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.calibration_load_failed", "session", input.Name, "session_id", input.Session.Metadata.ProviderSessionID(), slog.Any("err", err))
		return 0, err
	}
	if !ok {
		cliCompactLog.Logger().Info("cli.compact.calibration_auto_probe", "session", input.Name, "session_id", input.Session.Metadata.ProviderSessionID())
		_, _ = fmt.Fprintf(out, "no calibration on file; probing claude /context for static overhead (30-60 seconds)...\n")
		if err := runAutoCalibrate(cmd.Context(), out, input.Session, input.Model); err != nil {
			cliCompactLog.Logger().Error("cli.compact.calibration_auto_probe_failed", "session", input.Name, "session_id", input.Session.Metadata.ProviderSessionID(), "err", err)
			return 0, err
		}
		cal, ok, err = compactengine.LoadCalibration(input.Session.Metadata.ProviderSessionID())
		if err != nil || !ok {
			cliCompactLog.Logger().Error("cli.compact.calibration_post_probe_missing", "session", input.Name, "session_id", input.Session.Metadata.ProviderSessionID(), slog.Any("err", err))
			return 0, fmt.Errorf("auto-probe finished but calibration still missing for %q", input.Name)
		}
	}
	return cal.StaticOverhead, nil
}

func renderCompactUpfrontStats(
	ctx context.Context,
	out io.Writer,
	input compactCommandInput,
	slice *compactengine.Slice,
	mode Mode,
	staticOverhead int,
) UpfrontStats {
	if input.Target == 0 {
		return UpfrontStats{}
	}
	thinking, images, toolPairs, chatTurns := categoryCounts(slice)
	currentTotal, maxTokens := 0, 0
	calibDate := ""
	layer := contextusage.NewDefault(input.Session, input.Model, "")
	if usage, err := layer.Usage(ctx, contextusage.UsageOptions{MaxAge: 24 * time.Hour}); err == nil {
		currentTotal = usage.TotalTokens
		maxTokens = usage.MaxTokens
	}
	if cal, ok, _ := compactengine.LoadCalibration(input.Session.Metadata.ProviderSessionID()); ok {
		calibDate = cal.CapturedAt.UTC().Format("2006-01-02")
	}
	upfrontStats := UpfrontStats{
		SessionName:   input.Session.Name,
		SessionID:     input.Session.Metadata.ProviderSessionID(),
		Model:         input.ModelDisplay,
		Mode:          mode,
		CurrentTotal:  currentTotal,
		MaxTokens:     maxTokens,
		Target:        input.Target,
		StaticFloor:   staticOverhead,
		Reserved:      input.Reserved,
		Thinking:      thinking,
		Images:        images,
		ToolPairs:     toolPairs,
		ChatTurns:     chatTurns,
		StrippersText: strippersDescribe(input.Strippers),
		TargetDate:    calibDate,
	}
	if !isTerminal(out) {
		RenderUpfrontPanel(out, upfrontStats)
	}
	return upfrontStats
}

func newCompactCounter(input compactCommandInput) (compactengine.Counter, error) {
	if input.Target == 0 {
		return nil, nil
	}
	key, err := compactengine.AnthropicAPIKey()
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.api_key_failed", "session", input.Name, slog.Any("err", err))
		return nil, err
	}
	layer := contextusage.NewDefault(input.Session, input.Model, key)
	return &layerCounter{layer: layer, model: input.Model}, nil
}

func runCompactPlan(
	ctx context.Context,
	out io.Writer,
	input compactCommandInput,
	slice *compactengine.Slice,
	mode Mode,
	staticOverhead int,
	counter compactengine.Counter,
	upfrontStats UpfrontStats,
	jsonMode bool,
) (*compactengine.PlanResult, bool, *progressView, error) {
	cliCompactLog.Logger().Info("cli.compact.preview.run_plan.started", "session", input.Name, "target", input.Target, "mode", mode.Label())
	isTTY := isTerminal(out)
	var progress *progressView
	var onIter func(compactengine.IterationRecord)
	if jsonMode && input.Target > 0 {
		iterNum := 0
		onIter = func(rec compactengine.IterationRecord) {
			iterNum++
			_ = emitStreamEvent(out, IterationEvent{
				Event:        "iteration",
				IterNum:      iterNum,
				Step:         rec.Step,
				TailTokens:   rec.TailTokens,
				CtxTotal:     rec.CtxTotal,
				Projected:    rec.CtxTotal,
				Delta:        rec.Delta,
				CompactRunID: "",
			})
		}
	} else if input.Target > 0 {
		progress = newProgressView(out, input.Target, mode, isTTY, upfrontStats)
		onIter = progress.Update
	}
	planRes, err := compactengine.RunPlan(ctx, compactengine.PlanInput{
		Slice:          slice,
		Strippers:      input.Strippers,
		Target:         input.Target,
		StaticOverhead: staticOverhead,
		Reserved:       input.Reserved,
		Counter:        counter,
		OnIteration:    onIter,
	})
	if progress != nil {
		progress.Finish()
	}
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.preview.run_plan.failed", "session", input.Name, "err", err)
		return nil, isTTY, progress, err
	}
	cliCompactLog.Logger().Info("cli.compact.preview.run_plan.completed", "session", input.Name, "hit_target", planRes.HitTarget)
	return planRes, isTTY, progress, nil
}

func renderCompactResult(
	out io.Writer,
	input compactCommandInput,
	slice *compactengine.Slice,
	planRes *compactengine.PlanResult,
	progress *progressView,
	staticOverhead int,
	isTTY bool,
) {
	if input.Target > 0 && isTTY && progress != nil {
		progress.Complete(planRes, staticOverhead, input.Reserved, input.Apply, input.Transcript)
	}
	if input.Target > 0 && input.ShowPasses && !isTTY {
		RenderIterationLog(out, planRes.Iterations)
	}
	if input.Target == 0 {
		RenderNoTarget(out, compactMode(input.Apply), input.Session.Name, input.Strippers, planRes, len(planRes.BoundaryTail), len(slice.PostBoundary))
		return
	}
	if !input.Apply && !isTTY {
		RenderFinalPreview(out, planRes, input.Target, staticOverhead, input.Reserved)
	}
}

func maybeSummarizeCompact(
	ctx context.Context,
	out io.Writer,
	input compactCommandInput,
	slice *compactengine.Slice,
	planRes *compactengine.PlanResult,
) {
	mode, err := compactengine.NormalizeSummarizeMode(input.SummarizeMode)
	if err != nil {
		cliCompactLog.Logger().Warn("cli.compact.summarize_mode_invalid", "session", input.Name, "err", err)
		return
	}
	decision, err := compactengine.DoSummarize(ctx, compactengine.SummarizeRequest{
		Session: input.Session,
		Slice:   slice,
		Options: planRes.Options,
		Model:   input.Model,
		Mode:    mode,
		Adapter: nil,
	})
	if err != nil {
		cliCompactLog.Logger().Warn("cli.compact.summarize_failed_continuing", "session", input.Name, "err", err)
		_, _ = fmt.Fprintf(out, "summary failed (%v); applying without summary\n", err)
		return
	}
	if !decision.ShouldSummarize {
		cliCompactLog.Logger().Info("cli.compact.summarize_skipped", "session", input.Name, "mode", mode, "reason", decision.Reason)
		return
	}
	_, _ = fmt.Fprintln(out, "summarizing dropped content via provider adapter (30-60s)...")
	if decision.Summary != "" {
		planRes.Options.Summary = decision.Summary
		planRes.BoundaryTail = compactengine.Synthesize(slice, planRes.Options)
		cliCompactLog.Logger().Info("cli.compact.summarize_injected", "session", input.Name, "summary_bytes", len(decision.Summary))
	}
}
