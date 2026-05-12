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
		categories := make([]RuntimeUsageCategory, 0, len(usage.Categories))
		for _, cat := range usage.Categories {
			categories = append(categories, RuntimeUsageCategory{
				Name:       cat.Name,
				Tokens:     cat.Tokens,
				IsDeferred: cat.IsDeferred,
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
	var counter Counter
	if req.TargetTokens > 0 {
		built, counterErr := buildProberCounter(req, modelForCount)
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
		Strippers:      req.Strippers,
		Target:         req.TargetTokens,
		StaticOverhead: staticOverhead,
		Reserved:       req.Reserved,
		Counter:        counter,
		Out:            nil,
		OnIteration: func(r IterationRecord) {
			iterCount++
			if onIteration != nil {
				onIteration(RuntimeIteration{Iteration: r, Accepted: true})
			}
		},
		BatchSize:     0,
		ChatBatchSize: 0,
		StopTimeout:   0,
		CompactRunID:  "",
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
		summaryMode := req.SummarizeMode
		if summaryMode == "" {
			summaryMode = SummarizeModeFromLegacy(req.Summarize, req.Summarize)
		}
		decision, summaryErr := DoSummarize(ctx, SummarizeRequest{
			Session: req.Session,
			Slice:   slice,
			Options: planRes.Options,
			Model:   modelForCount,
			Mode:    summaryMode,
			Adapter: nil,
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
		} else if decision.Summary != "" {
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
		in := ApplyInput{
			Slice:           slice,
			SessionID:       req.Session.Metadata.ProviderSessionID(),
			Cwd:             req.Session.Metadata.WorkspaceRoot,
			Version:         "clyde",
			Strippers:       req.Strippers,
			Target:          req.TargetTokens,
			BoundaryTail:    planRes.BoundaryTail,
			PreCompactTok:   planRes.BaselineTail,
			FinalProjection: finalProjection(planRes, staticOverhead, req.Reserved),
			ForceOverTarget: req.ForceOverTarget,
		}
		applyRes, applyErr := Apply(in)
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
		result.Apply = applyRes
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

// buildProberCounter wires the planner to the registered
// CandidateProber for the session's provider. The optional debug
// cross-check against Anthropic count_tokens is enabled only when the
// CLYDE_COMPACT_DEBUG_COUNT_TOKENS env var is set and an API key is
// available; the debug counter is never authoritative.
func buildProberCounter(req RuntimeRequest, modelForCount string) (Counter, error) {
	providerID := string(req.Session.ProviderID())
	prober, ok := contextusage.GetCandidate(providerID)
	if !ok {
		return nil, fmt.Errorf("compact: no candidate prober registered for provider %q", providerID)
	}
	cfg := proberCounterConfig{
		Prober:         prober,
		SessionID:      req.Session.Metadata.ProviderSessionID(),
		TranscriptPath: req.Session.Metadata.ProviderTranscriptPath(),
		WorkDir:        req.Session.Metadata.WorkspaceRoot,
		Cwd:            req.Session.Metadata.WorkspaceRoot,
		Version:        "clyde",
		Debug:          nil,
	}
	if os.Getenv("CLYDE_COMPACT_DEBUG_COUNT_TOKENS") == "1" {
		if key, keyErr := AnthropicAPIKey(); keyErr == nil && strings.TrimSpace(modelForCount) != "" {
			cfg.Debug = NewTokenCounter(key, modelForCount)
		} else if keyErr != nil {
			compactLog.Logger().Debug("compact.runtime.debug_counter_disabled",
				"component", "compact",
				"subcomponent", "runtime",
				"reason", "api_key_unavailable",
				"err", keyErr.Error(),
			)
		}
	}
	return newProberCounter(cfg), nil
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
