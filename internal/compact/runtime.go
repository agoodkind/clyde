package compact

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
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
	upfront := RuntimeUpfront{
		SessionName:   req.Session.Name,
		SessionID:     req.Session.Metadata.ProviderSessionID(),
		Model:         modelForRender,
		Target:        req.TargetTokens,
		Reserved:      req.Reserved,
		Thinking:      thinking,
		Images:        images,
		ToolPairs:     toolPairs,
		ChatTurns:     chatTurns,
		StrippersText: strippersDescribe(req.Strippers),
	}
	usage, usageErr := ProbeContextUsage(ctx, ProbeOptions{
		SessionID:   req.Session.Metadata.ProviderSessionID(),
		WorkDir:     req.Session.Metadata.WorkDir,
		Timeout:     30 * time.Second,
		ForkSession: true,
	})
	if usageErr == nil {
		upfront.CurrentTotal = usage.TotalTokens
		upfront.MaxTokens = usage.MaxTokens
	}
	staticOverhead := 0
	if req.TargetTokens > 0 {
		cal, ok, calErr := LoadCalibration(req.Session.Metadata.ProviderSessionID())
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
		if ok {
			staticOverhead = cal.StaticOverhead
			upfront.TargetDate = cal.CapturedAt.UTC().Format("2006-01-02")
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
		key, keyErr := AnthropicAPIKey()
		if keyErr != nil {
			compactLog.Logger().Error("compact.runtime.api_key_missing",
				"component", "compact",
				"subcomponent", "runtime",
				"session", req.Session.Name,
				"session_id", req.Session.Metadata.ProviderSessionID(),
				"err", keyErr.Error(),
			)
			return nil, keyErr
		}
		counter = &runtimeLayerCounter{counter: NewTokenCounter(key, modelForCount)}
	}

	var iterCount int
	planRes, err := RunPlan(ctx, PlanInput{
		Slice:          slice,
		Strippers:      req.Strippers,
		Target:         req.TargetTokens,
		StaticOverhead: staticOverhead,
		Reserved:       req.Reserved,
		Counter:        counter,
		OnIteration: func(r IterationRecord) {
			iterCount++
			if onIteration != nil {
				onIteration(RuntimeIteration{Iteration: r, Accepted: true})
			}
		},
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
			Slice:         slice,
			SessionID:     req.Session.Metadata.ProviderSessionID(),
			Cwd:           req.Session.Metadata.WorkspaceRoot,
			Version:       "clyde",
			Strippers:     req.Strippers,
			Target:        req.TargetTokens,
			BoundaryTail:  planRes.BoundaryTail,
			PreCompactTok: planRes.BaselineTail,
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

type runtimeLayerCounter struct {
	counter *TokenCounter
}

func (c *runtimeLayerCounter) CountSyntheticUser(ctx context.Context, contentArray []OutputBlock) (int, error) {
	return c.counter.CountSyntheticUser(ctx, contentArray)
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
