package compact

import (
	"context"
	"fmt"
	"io"

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
}

func runCompactViaDaemon(ctx context.Context, out io.Writer, in compactDaemonRunInput) error {
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
		return err
	}
	defer cancel()

	var upfront UpfrontStats
	var haveUpfront bool
	var progress *progressView
	iterations := make([]compactengine.IterationRecord, 0, 64)
	var final *clydev1.CompactFinal
	var mutation *clydev1.CompactApplyMutation

	for ev := range events {
		switch ev.GetKind() {
		case clydev1.CompactEvent_KIND_UPFRONT:
			u := ev.GetUpfront()
			if u == nil {
				continue
			}
			upfront = UpfrontStats{
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
				StrippersText: u.GetStrippersText(),
				TargetDate:    u.GetCalibrationDate(),
			}
			haveUpfront = true
			if in.IsTTY {
				if progress == nil {
					progress = newProgressView(out, in.Target, in.Mode, true, upfront)
				}
			} else {
				RenderUpfrontPanel(out, upfront)
			}
		case clydev1.CompactEvent_KIND_ITERATION:
			it := ev.GetIteration()
			if it == nil {
				continue
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
			}
			iterations = append(iterations, rec)
			if progress != nil {
				progress.Update(rec)
			}
		case clydev1.CompactEvent_KIND_FINAL:
			final = ev.GetFinal()
		case clydev1.CompactEvent_KIND_APPLY_MUTATION:
			mutation = ev.GetApplyMutation()
		}
	}
	if done != nil {
		if streamErr := <-done; streamErr != nil {
			return streamErr
		}
	}

	if final == nil {
		return fmt.Errorf("compact daemon stream ended without final result")
	}
	if !haveUpfront {
		return fmt.Errorf("compact daemon stream ended without upfront stats")
	}

	planRes := &compactengine.PlanResult{
		BaselineTail: int(final.GetBaselineTail()),
		FinalTail:    int(final.GetFinalTail()),
		HitTarget:    final.GetHitTarget(),
		Iterations:   iterations,
	}

	if progress != nil {
		progress.Finish()
		progress.Complete(planRes, int(final.GetStaticFloor()), int(final.GetReservedTokens()), in.Mode == ModeApply, final.GetTranscriptPath())
	} else if in.ShowPasses {
		RenderIterationLog(out, iterations)
	}

	if in.Mode == ModePreview {
		if !in.IsTTY {
			RenderFinalPreview(out, planRes, in.Target, int(final.GetStaticFloor()), int(final.GetReservedTokens()))
		}
		return nil
	}

	if mutation != nil {
		_, _ = fmt.Fprintln(out, "  verified appended transcript lines as valid JSON boundary/synthetic pair")
		_, _ = fmt.Fprintf(out, "\napplied:\n  boundary uuid:   %s\n  synthetic uuid:  %s\n  pre-apply bytes: %d\n  post-apply bytes: %d\n  snapshot:        %s\n  ledger:          %s\n",
			mutation.GetBoundaryUuid(), mutation.GetSyntheticUuid(), mutation.GetPreApplyOffset(),
			mutation.GetPostApplyOffset(), mutation.GetSnapshotPath(), mutation.GetLedgerPath())
	}
	if !in.IsTTY {
		RenderFinalApply(out, planRes, in.Target, int(final.GetStaticFloor()), int(final.GetReservedTokens()), final.GetTranscriptPath())
	}
	return nil
}
