package compact

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	compactengine "goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/daemon"
)

type compactDaemonRunInput struct {
	SessionName    string
	Mode           Mode
	Target         int
	Reserved       int
	Model          string
	ModelExplicit  bool
	Strippers      compactengine.Strippers
	Summarize      bool
	SummarizeMode  string
	Force          bool
	ShowPasses     bool
	IsTTY          bool
	TranscriptPath string
	JSONMode       bool
	CompactRunID   string
}

// compactDaemonStreamState holds the running state accumulated while
// consuming events from a CompactPreview or CompactApply stream.
type compactDaemonStreamState struct {
	upfront             UpfrontStats
	haveUpfront         bool
	progress            *progressView
	iterations          []compactengine.IterationRecord
	final               *clydev1.CompactFinal
	mutation            *clydev1.CompactApplyMutation
	iterEmit            int
	postBoundaryEntries int
}

func runCompactViaDaemon(ctx context.Context, out io.Writer, in compactDaemonRunInput) error {
	events, done, cancel, err := openCompactDaemonStream(ctx, in)
	if err != nil {
		return err
	}
	defer cancel()

	state := &compactDaemonStreamState{
		upfront: UpfrontStats{
			SessionName:   "",
			SessionID:     "",
			Model:         "",
			Mode:          in.Mode,
			CurrentTotal:  0,
			MaxTokens:     0,
			Target:        0,
			StaticFloor:   0,
			Reserved:      0,
			Thinking:      0,
			Images:        0,
			ToolPairs:     0,
			ChatTurns:     0,
			BaselineTail:  0,
			StrippersText: "",
			TargetDate:    "",
		},
		haveUpfront:         false,
		progress:            nil,
		iterations:          make([]compactengine.IterationRecord, 0, 64),
		final:               nil,
		mutation:            nil,
		iterEmit:            0,
		postBoundaryEntries: 0,
	}
	for ev := range events {
		consumeCompactEvent(out, in, state, ev)
	}
	if done != nil {
		if streamErr := <-done; streamErr != nil {
			return streamErr
		}
	}
	if state.final == nil {
		return fmt.Errorf("compact daemon stream ended without final result")
	}
	if !state.haveUpfront {
		return fmt.Errorf("compact daemon stream ended without upfront stats")
	}

	planRes := &compactengine.PlanResult{
		Options: compactengine.SynthOptions{
			DropThinking:         false,
			ImagesAsPlaceholder:  false,
			ToolDefault:          compactengine.ToolDetailFull,
			ToolDetailOverride:   nil,
			DroppedChatEntries:   nil,
			DroppedSummaryChunks: nil,
			TruncTokens:          0,
			Summary:              "",
		},
		BaselineTail: int(state.final.GetBaselineTail()),
		FinalTail:    int(state.final.GetFinalTail()),
		Iterations:   state.iterations,
		HitTarget:    state.final.GetHitTarget(),
		BoundaryTail: nil,
	}

	if in.JSONMode {
		return emitFinalResultJSON(out, in, planRes)
	}
	renderCompactDaemonResult(out, in, state, planRes)
	return nil
}

// openCompactDaemonStream opens a CompactPreview or CompactApply
// stream against the daemon and wraps any open error with a
// [slog.Error] breadcrumb so the failure is captured even when the
// caller decides not to surface it.
func openCompactDaemonStream(
	ctx context.Context,
	in compactDaemonRunInput,
) (<-chan *clydev1.CompactEvent, <-chan error, context.CancelFunc, error) {
	streamInput := daemon.CompactRunOptions{
		SessionName:    in.SessionName,
		TargetTokens:   in.Target,
		ReservedTokens: in.Reserved,
		Model:          in.Model,
		ModelExplicit:  in.ModelExplicit,
		Thinking:       in.Strippers.Thinking,
		Images:         in.Strippers.Images,
		Tools:          in.Strippers.Tools,
		Chat:           in.Strippers.Chat,
		Summarize:      in.Summarize,
		SummarizeMode:  in.SummarizeMode,
		Force:          in.Force,
		Refresh:        false,
	}
	var events <-chan *clydev1.CompactEvent
	var done <-chan error
	var cancel context.CancelFunc
	var err error
	if in.Mode == ModeApply {
		events, done, cancel, err = daemon.CompactApplyViaDaemon(ctx, streamInput)
	} else {
		events, done, cancel, err = daemon.CompactPreviewViaDaemon(ctx, streamInput)
	}
	if err != nil {
		slog.ErrorContext(ctx, "cli.compact.daemon_stream_open_failed",
			"component", "cli",
			"subcomponent", "compact",
			"session", in.SessionName,
			"mode", in.Mode.Label(),
			"err", err.Error(),
		)
		return nil, nil, nil, fmt.Errorf("open compact daemon stream: %w", err)
	}
	return events, done, cancel, nil
}

func consumeCompactEvent(out io.Writer, in compactDaemonRunInput, state *compactDaemonStreamState, ev *clydev1.CompactEvent) {
	switch ev.GetKind() {
	case clydev1.CompactEvent_KIND_UNSPECIFIED, clydev1.CompactEvent_KIND_STATUS:
		return
	case clydev1.CompactEvent_KIND_UPFRONT:
		handleUpfrontEvent(out, in, state, ev.GetUpfront())
	case clydev1.CompactEvent_KIND_ITERATION:
		handleIterationEvent(out, in, state, ev.GetIteration())
	case clydev1.CompactEvent_KIND_FINAL:
		state.final = ev.GetFinal()
	case clydev1.CompactEvent_KIND_APPLY_MUTATION:
		state.mutation = ev.GetApplyMutation()
	}
}

func handleUpfrontEvent(out io.Writer, in compactDaemonRunInput, state *compactDaemonStreamState, u *clydev1.CompactUpfront) {
	if u == nil {
		return
	}
	state.upfront = UpfrontStats{
		SessionName:   u.GetSessionName(),
		SessionID:     u.GetSessionId(),
		Model:         u.GetModel(),
		Mode:          in.Mode,
		CurrentTotal:  int(u.GetCurrentTotal()),
		MaxTokens:     int(u.GetMaxTokens()),
		Target:        int(u.GetTargetTokens()),
		StaticFloor:   int(u.GetStaticFloor()),
		Reserved:      int(u.GetReservedTokens()),
		Thinking:      int(u.GetThinkingBlocks()),
		Images:        int(u.GetImageBlocks()),
		ToolPairs:     int(u.GetToolPairs()),
		ChatTurns:     int(u.GetChatTurns()),
		BaselineTail:  0,
		StrippersText: u.GetStrippersText(),
		TargetDate:    u.GetCalibrationDate(),
	}
	state.haveUpfront = true
	state.postBoundaryEntries = int(u.GetPostBoundaryEntries())
	if in.Target <= 0 || in.JSONMode {
		return
	}
	if in.IsTTY {
		if state.progress == nil {
			state.progress = newProgressView(out, in.Target, in.Mode, true, state.upfront)
		}
		return
	}
	RenderUpfrontPanel(out, state.upfront)
}

func handleIterationEvent(out io.Writer, in compactDaemonRunInput, state *compactDaemonStreamState, it *clydev1.CompactIteration) {
	if it == nil {
		return
	}
	rec := compactengine.IterationRecord{
		Step:              it.GetStep(),
		TailTokens:        int(it.GetTailTokens()),
		CtxTotal:          int(it.GetCtxTotal()),
		Delta:             int(it.GetDelta()),
		ThinkingDropped:   it.GetThinkingDropped(),
		ImagesPlaceholder: it.GetImagesPlaceholder(),
		ToolsFull:         int(it.GetToolsFull()),
		ToolsLineOnly:     int(it.GetToolsLineOnly()),
		ToolsDropped:      int(it.GetToolsDropped()),
		ChatTurnsTotal:    int(it.GetChatTurnsTotal()),
		ChatTurnsDropped:  int(it.GetChatTurnsDropped()),
		Probe:             it.GetProbe(),
	}
	state.iterations = append(state.iterations, rec)
	if in.JSONMode && in.Target > 0 {
		state.iterEmit++
		_ = emitStreamEvent(out, IterationEvent{
			Event:        "iteration",
			IterNum:      state.iterEmit,
			Step:         rec.Step,
			TailTokens:   rec.TailTokens,
			CtxTotal:     rec.CtxTotal,
			Projected:    rec.CtxTotal,
			Delta:        rec.Delta,
			CompactRunID: in.CompactRunID,
		})
	}
	if state.progress != nil {
		state.progress.Update(rec)
	}
}

func emitFinalResultJSON(out io.Writer, in compactDaemonRunInput, planRes *compactengine.PlanResult) error {
	return emitStreamEvent(out, ResultEvent{
		Event:        "result",
		HitTarget:    planRes.HitTarget,
		BaselineTail: planRes.BaselineTail,
		FinalTail:    planRes.FinalTail,
		Target:       in.Target,
		CompactRunID: in.CompactRunID,
	})
}

func renderCompactDaemonResult(
	out io.Writer,
	in compactDaemonRunInput,
	state *compactDaemonStreamState,
	planRes *compactengine.PlanResult,
) {
	final := state.final
	if state.progress != nil {
		state.progress.Finish()
		state.progress.Complete(planRes, int(final.GetStaticFloor()), int(final.GetReservedTokens()), in.Mode == ModeApply, final.GetTranscriptPath())
	} else if in.Target > 0 && in.ShowPasses {
		RenderIterationLog(out, state.iterations)
	}

	if in.Mode == ModePreview {
		if in.Target > 0 && !in.IsTTY {
			RenderFinalPreview(out, planRes, in.Target, int(final.GetStaticFloor()), int(final.GetReservedTokens()))
		}
		return
	}

	if state.mutation != nil {
		renderApplyMutationBlock(out, state.mutation)
	}
	if in.Target > 0 && !in.IsTTY {
		RenderFinalApply(out, planRes, in.Target, int(final.GetStaticFloor()), int(final.GetReservedTokens()), final.GetTranscriptPath())
	}
}

func renderApplyMutationBlock(out io.Writer, mutation *clydev1.CompactApplyMutation) {
	_, _ = fmt.Fprintln(out, "  verified appended transcript lines as valid JSON boundary/synthetic pair")
	_, _ = fmt.Fprintf(out, "\napplied:\n  boundary uuid:   %s\n  synthetic uuid:  %s\n  pre-apply bytes: %d\n  post-apply bytes: %d\n  snapshot:        %s\n  ledger:          %s\n",
		mutation.GetBoundaryUuid(), mutation.GetSyntheticUuid(), mutation.GetPreApplyOffset(),
		mutation.GetPostApplyOffset(), mutation.GetSnapshotPath(), mutation.GetLedgerPath())
}
