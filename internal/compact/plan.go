package compact

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/contextcount"
)

// Strippers selects which categories the user wants to act on. The
// orchestrator demotes/strips in a fixed order: thinking, images,
// tools (full -> line-only -> drop), then chat.
type Strippers struct {
	Thinking bool
	Images   bool
	Tools    bool
	Chat     bool
}

// Any reports whether any stripper bit is set.
func (s *Strippers) Any() bool {
	if s == nil {
		return false
	}
	return s.Thinking || s.Images || s.Tools || s.Chat
}

// SetAll turns every bit on.
func (s *Strippers) SetAll() {
	s.Thinking = true
	s.Images = true
	s.Tools = true
	s.Chat = true
}

// PlanInput is the orchestrator's input bundle.
type PlanInput struct {
	Slice          *Slice
	Transcript     contextcount.Transcript
	Strippers      Strippers
	Target         int                   // required effective (billable) context total floor for the planned result
	StaticOverhead int                   // calibrated overhead used when projecting context total
	Reserved       int                   // reserved buffer (default 13_000)
	Counter        contextcount.Counter  // required for targeted planning
	Out            io.Writer             // fallback streaming sink when OnIteration nil
	OnIteration    func(IterationRecord) // preferred: called after each measure
	BatchSize      int                   // tool demotion batch size; default 8
	StopTimeout    time.Duration         // max wall time for whole loop; 0 = no limit
	// CompactRunID is the correlation id stamped on every
	// compact.plan.iteration slog event the planner emits. RunPlan
	// generates one with uuid.NewString when this is empty so every
	// run, including direct callers that did not set the field, gets
	// a unique id without further wiring.
	CompactRunID string
}

// PlanResult holds the final synthesis options plus the iteration log
// and the last measured tail token count. Apply consumes this.
type PlanResult struct {
	Options      SynthOptions
	BaselineTail int
	FinalTail    int
	Iterations   []IterationRecord
	HitTarget    bool
	BoundaryTail []OutputBlock // the synthesized content array
}

// IterationRecord is one row of the iteration log printed in the
// preview output. It also carries the current category-breakdown
// counts so the CLI can render a live dashboard showing exactly what
// has been stripped as the loop progresses.
type IterationRecord struct {
	Step       string // human-readable description
	TailTokens int    // Prober projection after this step
	CtxTotal   int    // static + tail + reserved
	Delta      int    // ctx - target (negative = OK)

	// ThinkingDropped is true once DropThinking is on.
	ThinkingDropped bool
	// ImagesPlaceholder is true once ImagesAsPlaceholder is on.
	ImagesPlaceholder bool

	// Tool pair counts by current fidelity level. Sum equals the
	// total number of tool pairs in PostBoundary.
	ToolsFull     int
	ToolsLineOnly int
	ToolsDropped  int

	// Chat turn counts. Total is the count of user/assistant turns
	// in PostBoundary. Dropped grows as the planner drops oldest
	// turns; Refine may un-drop some at the tail of the run.
	ChatTurnsTotal   int
	ChatTurnsDropped int

	// Probe is true when this record represents a mid-bisect
	// measurement rather than an accepted iteration. Probe rows
	// inform the dashboard about candidate measurements without
	// implying the planner committed to them. Accepted rows have
	// Probe=false.
	Probe bool
}

// RunPlan drives the target loop. Target must be greater than zero.
// The planner asks the configured counter for a projection after each
// demotion batch, and stops when it reaches target exactly or cannot
// reduce further without choosing a result smaller than target.
func RunPlan(ctx context.Context, in PlanInput) (*PlanResult, error) {
	if err := normalizePlanInput(&in); err != nil {
		return nil, err
	}
	if in.CompactRunID == "" {
		in.CompactRunID = uuid.NewString()
	}

	if in.StopTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, in.StopTimeout)
		defer cancel()
	}

	const maxUnpackLayers = 8
	for layer := 0; ; layer++ {
		runnerOpts := newSynthOptions()
		runner := newPlanRunner(in, runnerOpts)
		result, err := runner.runTarget(ctx)
		if err != nil {
			return nil, err
		}
		if result.HitTarget {
			return result, nil
		}
		if layer >= maxUnpackLayers {
			return result, nil
		}
		if !hasUnpackableSynthetic(in.Slice) {
			return result, nil
		}
		in.Slice = Rehydrate(in.Slice, 1)
		compactLog.Logger().Debug("compact.plan.unpacked_layer",
			"component", "compact",
			"subcomponent", "plan",
			"layer", layer+1,
			"compact_run_id", in.CompactRunID,
		)
	}
}

func hasUnpackableSynthetic(slice *Slice) bool {
	if slice == nil {
		return false
	}
	for _, entry := range slice.PostBoundary {
		if entry.IsSummary && entry.RehydratedFrom < 0 {
			return true
		}
	}
	return false
}

func normalizePlanInput(in *PlanInput) error {
	if in.Slice == nil {
		return fmt.Errorf("plan: nil slice")
	}
	if in.BatchSize <= 0 {
		in.BatchSize = 32
	}
	if in.Target <= 0 {
		return fmt.Errorf("plan: target must be greater than zero")
	}
	if in.Counter == nil {
		return fmt.Errorf("plan: target set but no token counter")
	}
	if len(in.Transcript.Messages) == 0 {
		transcript, err := buildPlanTranscriptFromSlice(in.Slice)
		if err != nil {
			return err
		}
		in.Transcript = transcript
	}
	return nil
}

func buildPlanTranscriptFromSlice(slice *Slice) (contextcount.Transcript, error) {
	messages, err := unmarshalTranscriptMessages(slice.PostBoundary)
	if err != nil {
		slog.Warn("compact.plan.transcript_build_failed",
			"component", "compact",
			"subcomponent", "plan",
			"err", err,
		)
		return contextcount.Transcript{}, fmt.Errorf("plan: build transcript: %w", err)
	}
	return contextcount.Transcript{
		Model:    DefaultCountModel,
		System:   nil,
		Tools:    nil,
		Messages: messages,
	}, nil
}

func newSynthOptions() SynthOptions {
	return SynthOptions{
		DropThinking:         false,
		ImagesAsPlaceholder:  false,
		ToolDefault:          ToolDetailFull,
		ToolDetailOverride:   map[string]ToolDetail{},
		DroppedChatEntries:   map[int]bool{},
		DroppedSummaryChunks: map[int]map[string]bool{},
		TruncTokens:          0,
		Summary:              "",
	}
}

type planRunner struct {
	in             PlanInput
	opts           SynthOptions
	totalToolPairs int
	totalChatTurns int
	baseline       int
	tail           int
	ctxTotal       int
	log            []IterationRecord
}

func newPlanRunner(in PlanInput, opts SynthOptions) *planRunner {
	return &planRunner{
		in:             in,
		opts:           opts,
		totalToolPairs: len(in.Slice.PairIndex),
		totalChatTurns: countChatTurns(in.Slice),
		baseline:       0,
		tail:           0,
		ctxTotal:       0,
		log:            nil,
	}
}

func countChatTurns(slice *Slice) int {
	total := 0
	for _, entry := range slice.PostBoundary {
		if entry.Type == "user" || entry.Type == "assistant" {
			total++
		}
	}
	return total
}

func (r *planRunner) runTarget(ctx context.Context) (*PlanResult, error) {
	if err := r.measureBaseline(ctx); err != nil {
		return nil, err
	}
	if r.hitTarget() {
		return r.finalize(true), nil
	}
	if err := r.dropThinking(ctx); err != nil {
		return nil, err
	}
	if r.hitTarget() {
		return r.finalize(true), nil
	}
	if err := r.replaceImagesWithPlaceholders(ctx); err != nil {
		return nil, err
	}
	if r.hitTarget() {
		return r.finalize(true), nil
	}
	if r.in.Strippers.Tools {
		if err := r.runToolDemotions(ctx); err != nil {
			return nil, err
		}
		if r.hitTarget() {
			return r.finalize(true), nil
		}
	}
	if r.in.Strippers.Chat {
		if err := r.runChatDrops(ctx); err != nil {
			return nil, err
		}
	}
	return r.finalize(r.hitTarget()), nil
}

func (r *planRunner) measureBaseline(ctx context.Context) error {
	record, err := r.measure(ctx, "baseline (no transforms)")
	if err != nil {
		return err
	}
	r.tail = record.TailTokens
	r.ctxTotal = record.CtxTotal
	r.baseline = record.TailTokens
	r.accept(ctx, record)
	return nil
}

func (r *planRunner) measure(ctx context.Context, label string) (IterationRecord, error) {
	transcript := applySynthOptionsToTranscript(r.in.Transcript, r.in.Slice, r.opts)
	counterTokens, err := r.in.Counter.Count(ctx, transcript)
	if err != nil {
		slog.ErrorContext(ctx, "compact.plan.count_failed", "component", "compact", "step", label, "err", err)
		return IterationRecord{}, fmt.Errorf("counter count after %q: %w", label, err)
	}
	ctxTotal := r.in.StaticOverhead + counterTokens + r.in.Reserved
	toolCounts := r.countToolFidelity()
	chatDropped := len(r.opts.DroppedChatEntries) + droppedSummaryChunkCount(r.opts)

	record := IterationRecord{
		Step:              label,
		TailTokens:        counterTokens,
		CtxTotal:          ctxTotal,
		Delta:             counterTokens - r.in.Target,
		ThinkingDropped:   r.opts.DropThinking,
		ImagesPlaceholder: r.opts.ImagesAsPlaceholder,
		ToolsFull:         toolCounts.Full,
		ToolsLineOnly:     toolCounts.LineOnly,
		ToolsDropped:      toolCounts.Dropped,
		ChatTurnsTotal:    r.totalChatTurns,
		ChatTurnsDropped:  chatDropped,
		Probe:             false,
	}
	return record, nil
}

type toolFidelityCounts struct {
	Full     int
	LineOnly int
	Dropped  int
}

func (r *planRunner) countToolFidelity() toolFidelityCounts {
	counts := toolFidelityCounts{Full: 0, LineOnly: 0, Dropped: 0}
	for _, level := range r.opts.ToolDetailOverride {
		switch level {
		case ToolDetailFull:
			continue
		case ToolDetailLineOnly:
			counts.LineOnly++
		case ToolDetailDrop:
			counts.Dropped++
		}
	}
	counts.Full = r.totalToolPairs - counts.LineOnly - counts.Dropped
	return counts
}

func (r *planRunner) accept(ctx context.Context, record IterationRecord) {
	r.tail = record.TailTokens
	r.ctxTotal = record.CtxTotal
	r.log = append(r.log, record)
	slog.InfoContext(ctx, "compact.plan.iteration",
		"component", "compact",
		"subcomponent", "plan",
		"compact_run_id", r.in.CompactRunID,
		"iter_num", len(r.log),
		"step", record.Step,
		"tail_tokens", record.TailTokens,
		"ctx_total", record.CtxTotal,
		"projected", record.CtxTotal,
		"delta", record.Delta,
	)
	r.emitRecord(record)
}

func (r *planRunner) emitRecord(record IterationRecord) {
	if r.in.OnIteration != nil {
		r.in.OnIteration(record)
		return
	}
	if r.in.Out == nil {
		return
	}
	tag := "OK"
	if record.Delta > 0 {
		tag = fmt.Sprintf("+%d over", record.Delta)
	} else if record.Delta < 0 {
		tag = fmt.Sprintf("-%d under", -record.Delta)
	}
	prefix := "  iter  "
	if record.Probe {
		prefix = "  probe "
	}
	fmt.Fprintf(r.in.Out, "%s%-44s tail=%d  ctx=%d  %s\n",
		prefix, record.Step, record.TailTokens, record.CtxTotal, tag)
}

// emitProbe forwards a probe row to the iteration log without
// mutating planner state. The caller of Bisect uses this so probes
// reach the dashboard without being recorded as accepted iterations.
func (r *planRunner) emitProbe(ctx context.Context, record IterationRecord) {
	slog.DebugContext(ctx, "compact.plan.iteration",
		"component", "compact",
		"subcomponent", "plan",
		"compact_run_id", r.in.CompactRunID,
		"step", record.Step,
		"tail_tokens", record.TailTokens,
		"ctx_total", record.CtxTotal,
		"projected", record.CtxTotal,
		"delta", record.Delta,
		"probe", true,
	)
	r.emitRecord(record)
}

func (r *planRunner) hitTarget() bool {
	return r.ctxTotal <= r.in.Target
}

func (r *planRunner) finalize(hit bool) *PlanResult {
	return finalize(r.in, r.opts, r.baseline, r.tail, r.log, hit)
}

func (r *planRunner) dropThinking(ctx context.Context) error {
	if r.opts.DropThinking {
		return nil
	}
	r.opts.DropThinking = true
	record, err := r.measure(ctx, "drop thinking")
	if err != nil {
		return err
	}
	if record.CtxTotal < r.in.Target {
		r.opts.DropThinking = false
		return nil
	}
	r.accept(ctx, record)
	return nil
}

func (r *planRunner) replaceImagesWithPlaceholders(ctx context.Context) error {
	if !r.shouldReplaceImagesWithPlaceholders() {
		return nil
	}
	r.opts.ImagesAsPlaceholder = true
	record, err := r.measure(ctx, "replace images with placeholders")
	if err != nil {
		return err
	}
	if record.CtxTotal < r.in.Target {
		r.opts.ImagesAsPlaceholder = false
		return nil
	}
	r.accept(ctx, record)
	return nil
}

func (r *planRunner) shouldReplaceImagesWithPlaceholders() bool {
	imagesSelected := r.in.Strippers.Images || r.in.Strippers.Any() && allImpliedByTarget(r.in.Strippers)
	return imagesSelected && !r.opts.ImagesAsPlaceholder
}

func (r *planRunner) runToolDemotions(ctx context.Context) error {
	toolIDs := orderedToolUseIDs(r.in.Slice)
	nearTargetBrake := maxInt(20_000, r.in.Target/10)
	passes := []toolDemotionPass{
		{
			Detail:         ToolDetailLineOnly,
			RevertDetail:   ToolDetailFull,
			DeleteOnRevert: true,
			Label:          "tools full -> line-only",
		},
		{
			Detail:         ToolDetailDrop,
			RevertDetail:   ToolDetailLineOnly,
			DeleteOnRevert: false,
			Label:          "tools line-only -> drop",
		},
	}
	for _, pass := range passes {
		if err := r.runToolDemotionPass(ctx, toolIDs, nearTargetBrake, pass); err != nil {
			return err
		}
		if r.hitTarget() {
			return nil
		}
	}
	return nil
}

type toolDemotionPass struct {
	Detail         ToolDetail
	RevertDetail   ToolDetail
	DeleteOnRevert bool
	Label          string
}

func (r *planRunner) runToolDemotionPass(ctx context.Context, toolIDs []string, nearTargetBrake int, pass toolDemotionPass) error {
	index := 0
	lastStepUnits := 0
	lastStepAmount := 0
	for index < len(toolIDs) && !r.hitTarget() {
		batchSize := adaptiveToolBatchSize(r.in.BatchSize, r.ctxTotal-r.in.Target, nearTargetBrake, lastStepUnits, lastStepAmount)
		batchEnd := minInt(index+batchSize, len(toolIDs))
		stepUnits := batchEnd - index
		r.applyToolDetail(toolIDs[index:batchEnd], pass.Detail)
		record, err := r.measure(ctx, fmt.Sprintf("%s (oldest %d)", pass.Label, stepUnits))
		if err != nil {
			return err
		}
		if record.CtxTotal < r.in.Target {
			r.revertToolDetail(toolIDs[index:batchEnd], pass)
			if batchSize > 1 {
				lastStepUnits = 0
				lastStepAmount = 0
				continue
			}
			break
		}
		lastStepUnits = stepUnits
		lastStepAmount = maxInt(r.ctxTotal-record.CtxTotal, 0)
		r.accept(ctx, record)
		index = batchEnd
	}
	return nil
}

func adaptiveToolBatchSize(defaultBatchSize, deltaOver, nearTargetBrake, lastStepUnits, lastStepAmount int) int {
	batchSize := defaultBatchSize
	if lastStepUnits > 0 && lastStepAmount > 0 {
		tokensPerUnit := maxInt(lastStepAmount/lastStepUnits, 1)
		needed := maxInt(deltaOver/tokensPerUnit, 1)
		if needed < batchSize {
			batchSize = needed
		}
	}
	if deltaOver <= nearTargetBrake {
		batchSize = minInt(batchSize, 4)
	}
	if deltaOver <= nearTargetBrake/2 {
		batchSize = minInt(batchSize, 2)
	}
	if deltaOver <= nearTargetBrake/4 {
		batchSize = 1
	}
	return batchSize
}

func (r *planRunner) applyToolDetail(toolIDs []string, detail ToolDetail) {
	for _, id := range toolIDs {
		r.opts.ToolDetailOverride[id] = detail
	}
}

func (r *planRunner) revertToolDetail(toolIDs []string, pass toolDemotionPass) {
	for _, id := range toolIDs {
		if pass.DeleteOnRevert {
			delete(r.opts.ToolDetailOverride, id)
		} else {
			r.opts.ToolDetailOverride[id] = pass.RevertDetail
		}
	}
}

// runChatDrops finds the smallest prefix of chatDropOrder whose
// application brings the projected context total at or below target.
// The bisect probes ceil(log2(N))+1 times instead of the N times the
// linear scan used.
func (r *planRunner) runChatDrops(ctx context.Context) error {
	if r.hitTarget() {
		return nil
	}
	dropOrder := chatDropOrder(r.in.Slice)
	if len(dropOrder) == 0 {
		return nil
	}

	probe := func(ctx context.Context, k int) (int, error) {
		r.applyChatDropSteps(dropOrder[:k])
		record, err := r.measure(ctx, fmt.Sprintf("probe drop %d chat turns", k))
		r.revertChatDropSteps(dropOrder[:k])
		if err != nil {
			return 0, err
		}
		return record.CtxTotal, nil
	}

	axis := Axis{
		N:      len(dropOrder),
		Probe:  probe,
		Target: r.in.Target,
		Label:  func(k int) string { return fmt.Sprintf("probe drop %d chat turns", k) },
		Emit:   func(rec IterationRecord) { r.emitProbe(ctx, rec) },
		BuildRecord: func(label string, k int, ctxTotal int) IterationRecord {
			toolCounts := r.countToolFidelity()
			return IterationRecord{
				Step:              label,
				TailTokens:        ctxTotal,
				CtxTotal:          ctxTotal,
				Delta:             ctxTotal - r.in.Target,
				ThinkingDropped:   r.opts.DropThinking,
				ImagesPlaceholder: r.opts.ImagesAsPlaceholder,
				ToolsFull:         toolCounts.Full,
				ToolsLineOnly:     toolCounts.LineOnly,
				ToolsDropped:      toolCounts.Dropped,
				ChatTurnsTotal:    r.totalChatTurns,
				ChatTurnsDropped:  k,
				Probe:             true,
			}
		},
	}

	k, err := BisectMin(ctx, axis)
	if err != nil {
		return err
	}
	if k == 0 {
		return nil
	}
	r.applyChatDropSteps(dropOrder[:k])
	record, err := r.measure(ctx, fmt.Sprintf("drop oldest chat turns (%d)", k))
	if err != nil {
		return err
	}
	r.accept(ctx, record)
	return nil
}

func (r *planRunner) applyChatDropSteps(steps []chatDropStep) {
	for _, step := range steps {
		applyChatDropStep(r.opts.DroppedChatEntries, r.opts.DroppedSummaryChunks, step)
	}
}

func (r *planRunner) revertChatDropSteps(steps []chatDropStep) {
	for _, step := range steps {
		revertChatDropStep(r.opts.DroppedChatEntries, r.opts.DroppedSummaryChunks, step)
	}
}

// maxInt is a tiny helper to avoid importing golang.org/x/exp or
// introducing a generics constraint just for this file.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// allImpliedByTarget reports whether the orchestrator should treat
// Images as part of a target-driven sweep even when --images was not
// passed explicitly. True when --all was effectively set (every flag
// on). Conservative: only sweep images when the user opted in.
func allImpliedByTarget(s Strippers) bool {
	return s.Thinking && s.Images && s.Tools && s.Chat
}

// orderedToolUseIDs returns tool_use ids in entry-order, oldest
// first. Used by the demotion loop so older noise vanishes before
// newer context.
func orderedToolUseIDs(slice *Slice) []string {
	type idx struct {
		id      string
		entryIx int
		blockIx int
	}
	var rows []idx
	for ei, e := range slice.PostBoundary {
		for bi, b := range e.Content {
			if b.Type == "tool_use" && b.ToolUseID != "" {
				rows = append(rows, idx{id: b.ToolUseID, entryIx: ei, blockIx: bi})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].entryIx != rows[j].entryIx {
			return rows[i].entryIx < rows[j].entryIx
		}
		return rows[i].blockIx < rows[j].blockIx
	})
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.id
	}
	return out
}

// chatDropOrder returns post-boundary entry indexes for chat turns in
// drop-priority order: oldest first, but the most recent assistant
// turn AND its immediate preceding user turn are excluded so the
// model always sees the latest exchange verbatim.
type chatDropStep struct {
	EntryIdx int
}

func chatDropOrder(slice *Slice) []chatDropStep {
	chatEntries := []chatDropStep{}
	lastAssistant := -1
	for ei, e := range slice.PostBoundary {
		if e.Type == "user" && !isToolResultOnly(e) {
			chatEntries = append(chatEntries, chatDropStep{EntryIdx: ei})
		}
		if e.Type == "assistant" {
			chatEntries = append(chatEntries, chatDropStep{EntryIdx: ei})
			lastAssistant = ei
		}
	}
	preserve := map[int]bool{}
	if lastAssistant >= 0 {
		preserve[lastAssistant] = true
		// Find the most recent non-tool-result user entry before it.
		for ei := lastAssistant - 1; ei >= 0; ei-- {
			if slice.PostBoundary[ei].Type == "user" && !isToolResultOnly(slice.PostBoundary[ei]) {
				preserve[ei] = true
				break
			}
		}
	}
	out := make([]chatDropStep, 0, len(chatEntries))
	for _, step := range chatEntries {
		if preserve[step.EntryIdx] {
			continue
		}
		out = append(out, step)
	}
	return out
}

func droppedSummaryChunkCount(opts SynthOptions) int {
	total := 0
	for _, chunks := range opts.DroppedSummaryChunks {
		total += len(chunks)
	}
	return total
}

func applyChatDropStep(droppedEntries map[int]bool, droppedChunks map[int]map[string]bool, step chatDropStep) {
	compactLog.Logger().Debug("compact.plan.chat_drop_applied",
		"component", "compact",
		"subcomponent", "plan",
		"entry_index", step.EntryIdx,
		"summary_chunk", false,
	)
	droppedEntries[step.EntryIdx] = true
	if droppedChunks != nil {
		delete(droppedChunks, step.EntryIdx)
	}
}

func revertChatDropStep(droppedEntries map[int]bool, droppedChunks map[int]map[string]bool, step chatDropStep) {
	compactLog.Logger().Debug("compact.plan.chat_drop_reverted",
		"component", "compact",
		"subcomponent", "plan",
		"entry_index", step.EntryIdx,
		"summary_chunk", false,
	)
	delete(droppedEntries, step.EntryIdx)
	if droppedChunks != nil {
		delete(droppedChunks, step.EntryIdx)
	}
}

func finalize(in PlanInput, opts SynthOptions, baseline, finalTail int, log []IterationRecord, hit bool) *PlanResult {
	return &PlanResult{
		Options:      opts,
		BaselineTail: baseline,
		FinalTail:    finalTail,
		Iterations:   log,
		HitTarget:    hit,
		BoundaryTail: Synthesize(in.Slice, opts),
	}
}
