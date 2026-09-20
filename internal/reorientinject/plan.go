package reorientinject

import (
	"slices"
	"strings"

	"goodkind.io/clyde/internal/tokencount"
)

// plan is where the split divides the conversation, and what the injection
// retains.
type plan struct {
	Cut Cut
	// Retained is the text the injection inserts under the summary. It measures
	// strictly under the budget.
	Retained string
	// RetainedTokens is the counter's measure of Retained.
	RetainedTokens int
}

// planCut decides where the conversation divides. It counts from the newest
// message toward the oldest, one segment at a time, and retains every segment
// that fits whole. The boundary segment is the first that does not fit, and the
// plan retains that segment's tail.
//
// Message index 0 is never retained. The model summarizes it in every
// compaction, so a compaction always produces a summary of some content.
//
// ok is false when no segment fits the budget. The caller then forwards the
// request unmodified.
func planCut(
	request ParsedRequest,
	budget int,
	counter tokencount.Counter,
	include func(SegmentKind) bool,
) (plan, bool) {
	if budget <= 0 || request.InstructionStart <= 1 {
		return noPlan(), false
	}
	// The retained text adds a role heading per message, which the per-segment
	// count does not see. A first pass that overshoots lowers the planning
	// budget by the overage and plans again.
	planningBudget := budget
	for range planAttempts {
		candidate, ok := planOnce(request, planningBudget, counter, include)
		if !ok {
			return noPlan(), false
		}
		tokens := counter.Estimate(candidate.Retained)
		if tokens < budget {
			candidate.RetainedTokens = tokens
			return candidate, true
		}
		planningBudget -= tokens - budget + 1
		if planningBudget <= 0 {
			return noPlan(), false
		}
	}
	return noPlan(), false
}

// noPlan is the zero result planCut returns when no segment fits the budget.
func noPlan() plan {
	return plan{
		Cut:            Cut{MessageIndex: 0, SegmentIndex: 0, HeadRunes: 0},
		Retained:       "",
		RetainedTokens: 0,
	}
}

// planAttempts bounds the re-planning loop. Each pass lowers the budget by the
// measured overage, so the second pass fits in every observed case.
const planAttempts = 4

func planOnce(
	request ParsedRequest,
	budget int,
	counter tokencount.Counter,
	include func(SegmentKind) bool,
) (plan, bool) {
	used := 0
	cut := Cut{MessageIndex: -1, SegmentIndex: 0, HeadRunes: 0}

	for messageIndex := request.InstructionStart - 1; messageIndex >= 1; messageIndex-- {
		segments := request.Messages[messageIndex].Segments
		for segmentIndex, segment := range slices.Backward(segments) {
			if !include(segment.Kind) {
				continue
			}
			count := counter.Estimate(segment.Text)
			if used+count < budget {
				used += count
				cut = Cut{MessageIndex: messageIndex, SegmentIndex: segmentIndex, HeadRunes: 0}
				continue
			}
			head := headRunesThatFit(segment.Text, budget-used, counter)
			return finishPlan(request, Cut{
				MessageIndex: messageIndex,
				SegmentIndex: segmentIndex,
				HeadRunes:    head,
			}, include)
		}
	}
	if cut.MessageIndex < 0 {
		return noPlan(), false
	}
	return finishPlan(request, cut, include)
}

// headRunesThatFit returns how many leading runes of text the model summarizes.
// The remaining runes are the largest tail the counter measures under budget.
func headRunesThatFit(text string, budget int, counter tokencount.Counter) int {
	runes := []rune(text)
	low, high := 0, len(runes)
	tail := 0
	for low <= high {
		middle := (low + high) / 2
		if counter.Estimate(string(runes[len(runes)-middle:])) < budget {
			tail = middle
			low = middle + 1
			continue
		}
		high = middle - 1
	}
	return len(runes) - tail
}

// finishPlan assembles the retained text for a cut. planCut measures the result
// and re-plans when the role headings pushed it over the budget.
func finishPlan(
	request ParsedRequest,
	cut Cut,
	include func(SegmentKind) bool,
) (plan, bool) {
	retained := retainedText(request, cut, include)
	if strings.TrimSpace(retained) == "" {
		return noPlan(), false
	}
	return plan{Cut: cut, Retained: retained, RetainedTokens: 0}, true
}

// retainedText renders the retained conversation. It starts at the boundary
// segment's tail and runs to the message before the instruction region.
func retainedText(request ParsedRequest, cut Cut, include func(SegmentKind) bool) string {
	var builder strings.Builder
	for messageIndex := cut.MessageIndex; messageIndex < request.InstructionStart; messageIndex++ {
		message := request.Messages[messageIndex]
		written := 0
		for segmentIndex, segment := range message.Segments {
			if messageIndex == cut.MessageIndex && segmentIndex < cut.SegmentIndex {
				continue
			}
			if !include(segment.Kind) {
				continue
			}
			text := segment.Text
			if messageIndex == cut.MessageIndex && segmentIndex == cut.SegmentIndex {
				text = tailRunes(text, cut.HeadRunes)
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			if written == 0 {
				builder.WriteString("\n\n### ")
				builder.WriteString(message.Role.Heading())
				builder.WriteString("\n\n")
			} else {
				builder.WriteString("\n\n")
			}
			builder.WriteString(text)
			written++
		}
	}
	return strings.TrimSpace(builder.String())
}

// tailRunes returns text with its first headRunes runes removed.
func tailRunes(text string, headRunes int) string {
	if headRunes <= 0 {
		return text
	}
	runes := []rune(text)
	if headRunes >= len(runes) {
		return ""
	}
	return string(runes[headRunes:])
}
