package reorientinject

import (
	"slices"
	"strings"

	"goodkind.io/clyde/internal/reorienttag"
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
// The count includes the first message after RetainStart. After a compaction
// that message stores the prior summary and the prior injection, and a budget
// larger than every later message retains the tail of it. The model always
// summarizes some content: when every message fits the budget, the plan sends
// that first message whole and retains the rest.
//
// maxBytes caps the retained text in bytes; zero means no cap. ok is false
// when no segment fits the budget. The caller then forwards the request
// unmodified.
func planCut(
	request ParsedRequest,
	budget int,
	maxBytes int,
	counter tokencount.Counter,
	include func(SegmentKind) bool,
) (plan, bool) {
	if budget <= 0 || request.RetainStart < 0 || request.InstructionStart <= request.RetainStart {
		return noPlan(), false
	}
	// The segment count includes frames and the marker. The final check
	// measures all retained text. At or over the budget, planning subtracts the
	// overage and retries; a byte cap overage reduces the planning budget by
	// the same fraction.
	planningBudget := budget
	for range planAttempts {
		candidate, ok := planOnce(request, planningBudget, counter, include)
		if !ok {
			return noPlan(), false
		}
		tokens := counter.Estimate(candidate.Retained)
		if tokens < budget && (maxBytes <= 0 || len(candidate.Retained) <= maxBytes) {
			candidate.RetainedTokens = tokens
			return candidate, true
		}
		overage := tokens - budget + 1
		if maxBytes > 0 && len(candidate.Retained) > maxBytes {
			byteOverage := tokens - int(float64(tokens)*float64(maxBytes)/float64(len(candidate.Retained))) + 1
			overage = max(overage, byteOverage)
		}
		planningBudget -= overage
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
	// The count starts with the tokens in the format marker line, which begins
	// every retained text.
	used := counter.Estimate(reorienttag.TranscriptFormatMarker + "\n")
	cut := Cut{MessageIndex: -1, SegmentIndex: 0, HeadRunes: 0}

	for messageIndex := request.InstructionStart - 1; messageIndex >= request.RetainStart; messageIndex-- {
		message := request.Messages[messageIndex]
		for segmentIndex, segment := range slices.Backward(message.Segments) {
			if !include(segment.Kind) {
				continue
			}
			framed := frameSegment(message.Role, segment, segment.Text)
			count := counter.Estimate(reorienttag.EncodeSegment(framed))
			if used+count < budget {
				used += count
				cut = Cut{MessageIndex: messageIndex, SegmentIndex: segmentIndex, HeadRunes: 0}
				continue
			}
			// An atomic boundary segment stays whole in the forwarded request.
			// A head of its full length summarizes all of it and retains none.
			head := len([]rune(segment.Text))
			if !segment.Atomic {
				// The boundary segment tail receives the remaining budget minus
				// the token count of its empty frame.
				overhead := counter.Estimate(reorienttag.EncodeSegment(frameSegment(message.Role, segment, "")))
				head = headRunesThatFit(segment.Text, budget-used-overhead, counter)
			}
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
	if cut.MessageIndex == request.RetainStart && cut.HeadRunes == 0 {
		// Every message fits. The model still needs content to summarize, so
		// the forwarded request keeps the first counted message whole and the
		// plan retains the rest.
		if request.InstructionStart <= request.RetainStart+1 {
			return noPlan(), false
		}
		cut = Cut{MessageIndex: request.RetainStart + 1, SegmentIndex: 0, HeadRunes: 0}
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
	if retained == "" {
		return noPlan(), false
	}
	return plan{Cut: cut, Retained: retained, RetainedTokens: 0}, true
}

func retainedText(request ParsedRequest, cut Cut, include func(SegmentKind) bool) string {
	segments := make([]reorienttag.Segment, 0)
	for messageIndex := cut.MessageIndex; messageIndex < request.InstructionStart; messageIndex++ {
		message := request.Messages[messageIndex]
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
			segments = append(segments, frameSegment(message.Role, segment, text))
		}
	}
	return reorienttag.EncodeTranscript(segments)
}

func frameSegment(role Role, segment Segment, text string) reorienttag.Segment {
	return reorienttag.Segment{
		Role: frameRole(role),
		Kind: frameKind(role, segment.Kind),
		Text: text,
	}
}

func frameRole(role Role) reorienttag.SegmentRole {
	switch role {
	case RoleUser:
		return reorienttag.SegmentRoleUser
	case RoleAssistant:
		return reorienttag.SegmentRoleAssistant
	case RoleSystem:
		return reorienttag.SegmentRoleSystem
	case RoleOther:
		return reorienttag.SegmentRoleOther
	}
	return reorienttag.SegmentRoleOther
}

func frameKind(role Role, kind SegmentKind) reorienttag.SegmentKind {
	switch kind {
	case KindText:
		return textFrameKind(role)
	case KindThinking:
		return reorienttag.SegmentKindThinking
	case KindToolUse:
		return reorienttag.SegmentKindToolCall
	case KindToolResult:
		return reorienttag.SegmentKindToolOutput
	case KindImage, KindOther:
		return reorienttag.SegmentKindOther
	}
	return reorienttag.SegmentKindOther
}

func textFrameKind(role Role) reorienttag.SegmentKind {
	switch role {
	case RoleUser, RoleAssistant:
		return reorienttag.SegmentKindChat
	case RoleSystem:
		return reorienttag.SegmentKindSystem
	case RoleOther:
		return reorienttag.SegmentKindOther
	}
	return reorienttag.SegmentKindOther
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
