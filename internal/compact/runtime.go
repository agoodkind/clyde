package compact

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	"goodkind.io/clyde/internal/categorystyle"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/contextcount"
	"goodkind.io/clyde/internal/contextusage"
	"goodkind.io/clyde/internal/session"
	sessionsettings "goodkind.io/clyde/internal/session/settings"
)

func ResolveModelForCounting(store session.Store, sess *session.Session, fallback string) (string, string, string) {
	if strings.TrimSpace(fallback) == "" {
		fallback = DefaultCountModel
	}
	if sess != nil && sess.Metadata.ProviderTranscriptPath() != "" {
		rawModel, displayModel := extractRawModelAndFamily(sess.Metadata.ProviderTranscriptPath())
		rawModel = strings.TrimSpace(rawModel)
		if rawModel != "" {
			compactLog.Logger().Debug("compact.runtime.model_resolved",
				"component", "compact",
				"subcomponent", "runtime",
				"source", "transcript",
				"model", rawModel,
			)
			return rawModel, displayModel, "transcript"
		}
	}
	if store != nil && sess != nil && strings.TrimSpace(sess.Name) != "" {
		settings, err := sessionsettings.Load(store, sess)
		if err == nil && settings != nil && strings.TrimSpace(settings.Model) != "" {
			settingsModel := adaptercursor.NormalizeSessionSettingsModel(strings.TrimSpace(settings.Model))
			compactLog.Logger().Debug("compact.runtime.model_resolved",
				"component", "compact",
				"subcomponent", "runtime",
				"source", "settings",
				"model", settingsModel,
			)
			return settingsModel, settingsModel, "settings"
		}
	}
	compactLog.Logger().Debug("compact.runtime.model_resolved",
		"component", "compact",
		"subcomponent", "runtime",
		"source", "fallback",
		"model", fallback,
	)
	return fallback, fallback, "fallback"
}

func BuildRuntimeUpfront(ctx context.Context, req RuntimeRequest, modelForRender string) (RuntimeUpfront, int, *Slice, error) {
	if req.Session == nil {
		return RuntimeUpfront{}, 0, nil, fmt.Errorf("nil session")
	}
	if req.Reserved <= 0 {
		req.Reserved = 13_000
	}
	slice, err := LoadSlice(req.Session.Metadata.ProviderTranscriptPath())
	if err != nil {
		compactLog.Logger().Error("compact.runtime.upfront.load_slice_failed",
			"component", "compact",
			"subcomponent", "runtime",
			"session", req.Session.Name,
			"session_id", req.Session.Metadata.ProviderSessionID(),
			"transcript", req.Session.Metadata.ProviderTranscriptPath(),
			"err", err.Error(),
		)
		return RuntimeUpfront{}, 0, nil, err
	}
	slice = Rehydrate(slice, 8)
	thinking, images, toolPairs, chatTurns := categoryCounts(slice)
	transcriptPath := req.Session.Metadata.ProviderTranscriptPath()
	var fileSize int64
	if stat, statErr := os.Stat(transcriptPath); statErr == nil {
		fileSize = stat.Size()
	}
	upfront := RuntimeUpfront{
		SessionName:         req.Session.Name,
		SessionID:           req.Session.Metadata.ProviderSessionID(),
		Model:               modelForRender,
		CurrentTotal:        0,
		MaxTokens:           0,
		Messages:            0,
		CompactBuffer:       0,
		Free:                0,
		ContextOverhead:     0,
		Target:              req.TargetTokens,
		StaticFloor:         0,
		Reserved:            req.Reserved,
		Thinking:            thinking,
		Images:              images,
		ToolPairs:           toolPairs,
		ChatTurns:           chatTurns,
		StrippersText:       strippersDescribe(req.Strippers),
		TargetDate:          "",
		PostBoundaryEntries: len(slice.PostBoundary),
		Calibrated:          false,
		CalibrationOverhead: 0,
		TranscriptPath:      transcriptPath,
		FileSizeBytes:       fileSize,
		FileLineCount:       len(slice.AllEntries),
		HasBoundary:         slice.BoundaryLine >= 0,
		BoundaryLine:        slice.BoundaryLine,
		BoundaryUUID:        slice.BoundaryUUID,
		BoundaryTime:        slice.BoundaryTime.UTC(),
		UsagePercentage:     0,
		UsageAvailable:      false,
		UsageSource:         "",
		UsageCapturedAt:     time.Time{},
		UsageError:          "",
		UsageCategories:     nil,
	}
	usage, usageErr := probeSessionSnapshot(ctx, req.Session, req.Refresh)
	if usageErr != nil && req.TargetTokens > 0 {
		slog.ErrorContext(ctx, "compact.runtime.upfront.probe_required",
			"component", "compact",
			"subcomponent", "runtime",
			"session", req.Session.Name,
			"session_id", req.Session.Metadata.ProviderSessionID(),
			"target", req.TargetTokens,
			"err", usageErr.Error(),
		)
		return RuntimeUpfront{}, 0, nil, fmt.Errorf("compact: upfront /context probe is required for targeted run (target=%d): %w", req.TargetTokens, usageErr)
	}
	if usageErr == nil {
		upfront.CurrentTotal = usage.TotalTokens
		upfront.MaxTokens = usage.MaxTokens
		upfront.Messages = contextCategoryTokens(usage, "Messages")
		upfront.CompactBuffer = contextCategoryTokens(usage, "Compact buffer")
		upfront.Free = contextCategoryTokens(usage, "Free space")
		upfront.ContextOverhead = usage.StaticOverhead()
		upfront.UsageAvailable = true
		upfront.UsagePercentage = usage.Percentage
		upfront.UsageSource = "probe"
		upfront.UsageCapturedAt = compactClock.Now().UTC()
		providerID := string(req.Session.ProviderID())
		categories := make([]RuntimeUsageCategory, 0, len(usage.Categories))
		for _, cat := range usage.Categories {
			color, _ := categorystyle.ColorFor(providerID, cat.Name)
			categories = append(categories, RuntimeUsageCategory{
				Name:       cat.Name,
				Tokens:     cat.Tokens,
				IsDeferred: cat.IsDeferred,
				Color:      string(color),
			})
		}
		upfront.UsageCategories = categories
	} else {
		upfront.UsageError = usageErr.Error()
	}
	cal, calOK, calErr := LoadCalibration(req.Session.Metadata.ProviderSessionID())
	if calErr != nil {
		compactLog.Logger().Error("compact.runtime.upfront.calibration_load_failed",
			"component", "compact",
			"subcomponent", "runtime",
			"session", req.Session.Name,
			"session_id", req.Session.Metadata.ProviderSessionID(),
			"err", calErr.Error(),
		)
		return RuntimeUpfront{}, 0, nil, calErr
	}
	if calOK {
		upfront.Calibrated = true
		upfront.CalibrationOverhead = cal.StaticOverhead
		upfront.TargetDate = cal.CapturedAt.UTC().Format("2006-01-02")
	}
	staticOverhead := 0
	if req.TargetTokens > 0 {
		if calOK {
			staticOverhead = cal.StaticOverhead
		} else if upfront.ContextOverhead > 0 {
			staticOverhead = upfront.ContextOverhead
		}
	}
	upfront.StaticFloor = staticOverhead
	compactLog.Logger().Info("compact.runtime.upfront_built",
		"component", "compact",
		"subcomponent", "runtime",
		"session", req.Session.Name,
		"session_id", req.Session.Metadata.ProviderSessionID(),
		"model", modelForRender,
		"target", req.TargetTokens,
		"current_total", upfront.CurrentTotal,
		"static_floor", upfront.StaticFloor,
		"reserved", upfront.Reserved,
	)
	return upfront, staticOverhead, slice, nil
}

func RunRuntime(
	ctx context.Context,
	req RuntimeRequest,
	onIteration func(RuntimeIteration),
) (*RuntimeResult, error) {
	if req.Session == nil {
		return nil, fmt.Errorf("runtime: nil session")
	}
	if req.Reserved <= 0 {
		req.Reserved = 13_000
	}

	modelForCount := req.Model
	modelForRender := req.Model
	if !req.ModelExplicit {
		modelForCount, modelForRender, _ = ResolveModelForCounting(req.Store, req.Session, req.Model)
	}

	var upfront RuntimeUpfront
	var staticOverhead int
	var slice *Slice
	var err error
	if req.PreparedUpfront != nil && req.PreparedSlice != nil {
		upfront = *req.PreparedUpfront
		staticOverhead = req.PreparedStaticOverhead
		slice = req.PreparedSlice
	} else {
		upfront, staticOverhead, slice, err = BuildRuntimeUpfront(ctx, req, modelForRender)
		if err != nil {
			return nil, err
		}
	}
	compactLog.Logger().Info("compact.runtime.run_started",
		"component", "compact",
		"subcomponent", "runtime",
		"session", req.Session.Name,
		"session_id", req.Session.Metadata.ProviderSessionID(),
		"mode", req.Mode,
		"model", modelForCount,
		"target", req.TargetTokens,
	)
	var counter contextcount.Counter
	var transcript contextcount.Transcript
	if req.TargetTokens > 0 {
		transcript, err = BuildTranscript(slice, upfront, nil, nil)
		if err != nil {
			compactLog.Logger().Error("compact.runtime.transcript_build_failed",
				"component", "compact",
				"subcomponent", "runtime",
				"session", req.Session.Name,
				"session_id", req.Session.Metadata.ProviderSessionID(),
				"err", err.Error(),
			)
			return nil, err
		}
		transcript.Model = modelForCount
		built, counterErr := buildContextCounter(req)
		if counterErr != nil {
			compactLog.Logger().Error("compact.runtime.counter_init_failed",
				"component", "compact",
				"subcomponent", "runtime",
				"session", req.Session.Name,
				"session_id", req.Session.Metadata.ProviderSessionID(),
				"err", counterErr.Error(),
			)
			return nil, counterErr
		}
		counter = built
	}

	var iterCount int
	planRes, err := RunPlan(ctx, PlanInput{
		Slice:          slice,
		Transcript:     transcript,
		Strippers:      req.Strippers,
		Target:         req.TargetTokens,
		StaticOverhead: staticOverhead,
		Reserved:       req.Reserved,
		Counter:        counter,
		Out:            nil,
		OnIteration: func(r IterationRecord) {
			if !r.Probe {
				iterCount++
			}
			if onIteration != nil {
				onIteration(RuntimeIteration{Iteration: r, Accepted: !r.Probe})
			}
		},
		BatchSize:    0,
		StopTimeout:  0,
		CompactRunID: "",
	})
	if err != nil {
		compactLog.Logger().Error("compact.runtime.plan_failed",
			"component", "compact",
			"subcomponent", "runtime",
			"session", req.Session.Name,
			"session_id", req.Session.Metadata.ProviderSessionID(),
			"err", err.Error(),
		)
		return nil, err
	}

	result := &RuntimeResult{
		Upfront:        upfront,
		ModelForCount:  modelForCount,
		ModelForRender: modelForRender,
		Slice:          slice,
		Plan:           planRes,
		TranscriptPath: req.Session.Metadata.ProviderTranscriptPath(),
	}

	if req.Mode == RuntimeModeApply {
		applyRes, applyErr := runRuntimeApply(ctx, req, slice, planRes, modelForCount, staticOverhead)
		if applyErr != nil {
			return nil, applyErr
		}
		if applyRes != nil {
			result.Apply = applyRes
		}
	}
	compactLog.Logger().Info("compact.runtime.run_completed",
		"component", "compact",
		"subcomponent", "runtime",
		"session", req.Session.Name,
		"session_id", req.Session.Metadata.ProviderSessionID(),
		"mode", req.Mode,
		"hit_target", result.Plan.HitTarget,
		"baseline_tail", result.Plan.BaselineTail,
		"final_tail", result.Plan.FinalTail,
	)

	return result, nil
}

func runRuntimeApply(
	ctx context.Context,
	req RuntimeRequest,
	slice *Slice,
	planRes *PlanResult,
	modelForCount string,
	staticOverhead int,
) (*ApplyResult, error) {
	injectRuntimeSummary(ctx, req, slice, planRes, modelForCount)
	applyRes, applyErr := Apply(ApplyInput{
		Slice:           slice,
		SessionID:       req.Session.Metadata.ProviderSessionID(),
		Cwd:             req.Session.Metadata.WorkspaceRoot,
		Version:         "clyde",
		Strippers:       req.Strippers,
		Target:          req.TargetTokens,
		BoundaryTail:    planRes.BoundaryTail,
		PreCompactTok:   planRes.BaselineTail,
		Options:         planRes.Options,
		FinalProjection: finalProjection(planRes, staticOverhead, req.Reserved),
		ForceOverTarget: req.ForceOverTarget,
	})
	if applyErr != nil {
		compactLog.Logger().Error("compact.runtime.apply_failed",
			"component", "compact",
			"subcomponent", "runtime",
			"session", req.Session.Name,
			"session_id", req.Session.Metadata.ProviderSessionID(),
			"err", applyErr.Error(),
		)
		return nil, applyErr
	}
	if applyRes == nil || applyRes.NoOp {
		logRuntimeApplyNoOp(req)
		return nil, nil
	}
	if err := verifyPostApplyContext(ctx, req); err != nil {
		return nil, err
	}
	return applyRes, nil
}

func injectRuntimeSummary(
	ctx context.Context,
	req RuntimeRequest,
	slice *Slice,
	planRes *PlanResult,
	modelForCount string,
) {
	summaryMode := req.SummarizeMode
	if summaryMode == "" {
		summaryMode = SummarizeModeFromLegacy(req.Summarize, req.Summarize)
	}
	decision, summaryErr := DoSummarize(ctx, SummarizeRequest{
		Session:     req.Session,
		Slice:       slice,
		Options:     planRes.Options,
		Model:       modelForCount,
		Mode:        summaryMode,
		Adapter:     nil,
		DroppedText: "",
	})
	if summaryErr != nil {
		compactLog.Logger().Warn("compact.runtime.summarize_failed_continuing",
			"component", "compact",
			"subcomponent", "runtime",
			"session", req.Session.Name,
			"session_id", req.Session.Metadata.ProviderSessionID(),
			"mode", summaryMode,
			"reason", decision.Reason,
			"err", summaryErr,
		)
		return
	}
	if decision.Summary == "" {
		return
	}
	planRes.Options.Summary = decision.Summary
	planRes.BoundaryTail = Synthesize(slice, planRes.Options)
	compactLog.Logger().Info("compact.runtime.summarize_injected",
		"component", "compact",
		"subcomponent", "runtime",
		"session", req.Session.Name,
		"session_id", req.Session.Metadata.ProviderSessionID(),
		"mode", summaryMode,
		"reason", decision.Reason,
		"summary_bytes", len(decision.Summary),
	)
}

func logRuntimeApplyNoOp(req RuntimeRequest) {
	compactLog.Logger().Info("compact.runtime.apply_noop",
		"component", "compact",
		"subcomponent", "runtime",
		"session", req.Session.Name,
		"session_id", req.Session.Metadata.ProviderSessionID(),
		"target", req.TargetTokens,
	)
}

func verifyPostApplyContext(ctx context.Context, req RuntimeRequest) error {
	if req.TargetTokens <= 0 {
		return nil
	}
	postApplyUsage, postApplyErr := probeSessionSnapshot(ctx, req.Session, true)
	if postApplyErr != nil {
		slog.ErrorContext(ctx, "compact.runtime.post_apply_probe_failed",
			"component", "compact",
			"subcomponent", "runtime",
			"session", req.Session.Name,
			"session_id", req.Session.Metadata.ProviderSessionID(),
			"target", req.TargetTokens,
			"err", postApplyErr.Error(),
		)
		return fmt.Errorf("compact: post-apply context probe failed: %w", postApplyErr)
	}
	err := GuardRealContextOverTarget(
		req.Session.Metadata.ProviderSessionID(),
		req.TargetTokens,
		postApplyUsage.TotalTokens,
		req.ForceOverTarget,
	)
	if err != nil {
		compactLog.Logger().Error("compact.runtime.post_apply_over_target",
			"component", "compact",
			"subcomponent", "runtime",
			"session", req.Session.Name,
			"session_id", req.Session.Metadata.ProviderSessionID(),
			"target", req.TargetTokens,
			"actual", postApplyUsage.TotalTokens,
			"err", err.Error(),
		)
	}
	return err
}

func buildContextCounter(req RuntimeRequest) (contextcount.Counter, error) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.Error("compact.runtime.config_failed",
			"component", "compact",
			"subcomponent", "runtime",
			"err", err,
		)
		return nil, fmt.Errorf("load config: %w", err)
	}
	source := contextcount.CounterSource(cfg.Defaults.CompactCounter)
	counter, err := contextcount.NewCounter(source, contextcount.Deps{
		Access:      nil,
		HomeDir:     "",
		ProjectPath: req.Session.Metadata.WorkspaceRoot,
		WorkDir:     req.Session.Metadata.WorkspaceRoot,
		Version:     "clyde",
		Timeout:     0,
		Clock:       compactClock,
	})
	if err != nil {
		slog.Error("compact.runtime.counter_init_failed",
			"component", "compact",
			"subcomponent", "runtime",
			"counter_source", source,
			"err", err,
		)
		return nil, fmt.Errorf("context counter: %w", err)
	}
	return counter, nil
}

// finalProjection returns the planner's converged /context total
// projection. When the planner recorded at least one iteration, the
// last record's CtxTotal is the authoritative number; otherwise the
// projection is reconstructed from FinalTail plus the same static
// overhead and reserved buffer the planner used. Returns 0 when there
// is no plan input to gate on (target == 0 path).
func finalProjection(plan *PlanResult, staticOverhead, reserved int) int {
	if plan == nil {
		return 0
	}
	if len(plan.Iterations) > 0 {
		return plan.Iterations[len(plan.Iterations)-1].CtxTotal
	}
	if plan.FinalTail <= 0 {
		return 0
	}
	return staticOverhead + plan.FinalTail + reserved
}

var runtimeModelFamilyRegex = regexp.MustCompile(`claude-(?:\d+-)*(\w+)-\d+`)

func extractRawModelAndFamily(transcriptPath string) (string, string) {
	if strings.TrimSpace(transcriptPath) == "" {
		return "", ""
	}
	file, err := os.Open(transcriptPath)
	if err != nil {
		return "", ""
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 4*1024*1024)
	lastModel := ""
	for scanner.Scan() {
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if jsonErr := json.Unmarshal(scanner.Bytes(), &entry); jsonErr != nil {
			continue
		}
		if entry.Type == "assistant" {
			model := strings.TrimSpace(entry.Message.Model)
			if model != "" && model != "<synthetic>" {
				lastModel = model
			}
		}
	}
	if lastModel == "" {
		return "", ""
	}
	matches := runtimeModelFamilyRegex.FindStringSubmatch(lastModel)
	if len(matches) > 1 {
		return lastModel, matches[1]
	}
	return lastModel, lastModel
}

func categoryCounts(slice *Slice) (thinking, images, toolPairs, chatTurns int) {
	for _, e := range slice.PostBoundary {
		for _, b := range e.Content {
			switch b.Type {
			case "thinking", "redacted_thinking":
				thinking++
			case "image":
				images++
			}
		}
		if e.Type == "user" || e.Type == "assistant" {
			chatTurns++
		}
	}
	toolPairs = len(slice.PairIndex)
	return
}

func strippersDescribe(s Strippers) string {
	var parts []string
	if s.Thinking {
		parts = append(parts, "thinking")
	}
	if s.Images {
		parts = append(parts, "images")
	}
	if s.Tools {
		parts = append(parts, "tools")
	}
	if s.Chat {
		parts = append(parts, "chat")
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ",")
}

func contextCategoryTokens(usage contextusage.Snapshot, name string) int {
	total := 0
	for _, category := range usage.Categories {
		if category.Name == name {
			total += category.Tokens
		}
	}
	return total
}

// probeSessionSnapshot resolves the registered Prober for the
// session's provider id and asks it for a Snapshot. The function
// keeps the compact engine provider-neutral: it does not import any
// provider's spawn machinery and instead relies on the package-level
// registry the provider populated at init.
func probeSessionSnapshot(ctx context.Context, sess *session.Session, refresh bool) (contextusage.Snapshot, error) {
	prober, ok := contextusage.Get(string(sess.ProviderID()))
	if !ok {
		slog.WarnContext(ctx, "compact.runtime.probe_no_prober",
			"component", "compact",
			"subcomponent", "runtime",
			"provider", string(sess.ProviderID()),
			"session_id", sess.Metadata.ProviderSessionID(),
		)
		return contextusage.Snapshot{}, fmt.Errorf("contextusage: no prober registered for provider %q", sess.ProviderID())
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snapshot, err := prober.Probe(probeCtx, sess.Metadata.ProviderSessionID(), contextusage.ProbeOptions{
		RefreshHint: refresh,
		WorkDir:     sess.Metadata.WorkspaceRoot,
	})
	if err != nil {
		slog.WarnContext(ctx, "compact.runtime.probe_failed",
			"component", "compact",
			"subcomponent", "runtime",
			"session_id", sess.Metadata.ProviderSessionID(),
			"provider", string(sess.ProviderID()),
			"err", err.Error(),
		)
		return contextusage.Snapshot{}, fmt.Errorf("contextusage: probe %s: %w", sess.ProviderID(), err)
	}
	return snapshot, nil
}
