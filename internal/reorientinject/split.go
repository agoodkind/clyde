package reorientinject

// reorientSplitMinConversation is the fewest conversation messages (messages
// before the compaction prompt) that make a top/bottom split meaningful.
const reorientSplitMinConversation = 2

// splitPlan holds the tool-pairing-aware boundaries for one compaction request. The
// older half stays as [0:recentStart), the instruction region stays as
// [instructionStart:end), and the recent half [recentStart:instructionStart) is
// removed from the request and injected verbatim into the response.
type splitPlan struct {
	recentStart      int
	instructionStart int
}

// toolPairing indexes tool_use and tool_result blocks across the wire messages so
// the split keeps every pair together.
type toolPairing struct {
	// useIndex maps a tool_use id to the message index that declares it.
	useIndex map[string]int
	// results is, per message index, the tool_use ids its tool_result blocks answer.
	results [][]string
}

func indexToolPairs(messages []anthropicMessage) toolPairing {
	pairing := toolPairing{
		useIndex: make(map[string]int),
		results:  make([][]string, len(messages)),
	}
	for index := range messages {
		uses, results := messages[index].toolIDs()
		pairing.results[index] = results
		for _, id := range uses {
			pairing.useIndex[id] = index
		}
	}
	return pairing
}

// extendInstructionStart pulls the instruction region's start earlier to a fixpoint
// so every tool_result inside [start:total) has its tool_use inside the region too.
// Claude Code glues the compact prompt onto a tool_result message, so this keeps the
// paired tool_use assistant that precedes the prompt.
func extendInstructionStart(promptIndex int, total int, pairing toolPairing) int {
	start := promptIndex
	for {
		earliest := start
		for index := start; index < total; index++ {
			for _, useID := range pairing.results[index] {
				if useIndex, ok := pairing.useIndex[useID]; ok && useIndex < earliest {
					earliest = useIndex
				}
			}
		}
		if earliest == start {
			return start
		}
		start = earliest
	}
}

// planSplit computes the boundaries. It extends the instruction region back to keep
// tool pairs, then walks the recent half by count and byte cap, then snaps the older
// half to end on a user message so no tool call or system reminder is orphaned. ok
// is false when no valid split exists, so the caller falls back to no trim.
func planSplit(messages []anthropicMessage, promptIndex int, maxBytes int) (splitPlan, bool) {
	pairing := indexToolPairs(messages)
	instructionStart := extendInstructionStart(promptIndex, len(messages), pairing)
	conversationLen := instructionStart
	if conversationLen < reorientSplitMinConversation {
		return splitPlan{recentStart: 0, instructionStart: 0}, false
	}
	half := conversationLen / 2
	if half < 1 {
		return splitPlan{recentStart: 0, instructionStart: 0}, false
	}
	bytesUsed := 0
	recentStart := instructionStart
	taken := 0
	for index := instructionStart - 1; index >= 0 && taken < half; index-- {
		// Budget on the rendering the injection emits (renderBlocks includes tool_use
		// and tool_result), not text(), which drops tool blocks.
		size := len(messages[index].renderBlocks())
		if maxBytes > 0 && taken > 0 && bytesUsed+size > maxBytes {
			break
		}
		bytesUsed += size
		recentStart = index
		taken++
	}
	if recentStart >= instructionStart {
		return splitPlan{recentStart: 0, instructionStart: 0}, false
	}
	// Snap so the older half ends on a user message. A user message's tool_result
	// blocks answer the assistant just before it, so ending there closes that
	// exchange: no dangling tool_use, no trailing system reminder, and the join to the
	// instruction region's assistant stays valid (user -> assistant). The skipped
	// messages move into the recent half, which is injected verbatim.
	for recentStart > 0 && messages[recentStart-1].Role != "user" {
		recentStart--
	}
	if recentStart == 0 {
		return splitPlan{recentStart: 0, instructionStart: 0}, false
	}
	return splitPlan{recentStart: recentStart, instructionStart: instructionStart}, true
}

// trimKeepIndexes lists the message indexes that stay in the trimmed request: the
// older half [0:recentStart) plus the instruction region [instructionStart:total).
func trimKeepIndexes(recentStart int, instructionStart int, total int) []int {
	keep := make([]int, 0, total-(instructionStart-recentStart))
	for index := range recentStart {
		keep = append(keep, index)
	}
	for index := instructionStart; index < total; index++ {
		keep = append(keep, index)
	}
	return keep
}

// selectMessages returns the messages at the keep indexes, in order.
func selectMessages(messages []anthropicMessage, keep []int) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(keep))
	for _, index := range keep {
		out = append(out, messages[index])
	}
	return out
}

// validateTrim reports whether the kept messages form an Anthropic-valid sequence:
// tool-closed (every tool_result has its tool_use present), no unanswered tool_use,
// and no system message followed by a non-assistant except at the end. It is the
// hard gate that guarantees a trim is never forwarded unless it is valid.
func validateTrim(kept []anthropicMessage) bool {
	useIDs := make(map[string]bool)
	resultIDs := make(map[string]bool)
	for _, message := range kept {
		uses, results := message.toolIDs()
		for _, id := range uses {
			useIDs[id] = true
		}
		for _, id := range results {
			resultIDs[id] = true
		}
	}
	for id := range resultIDs {
		if !useIDs[id] {
			return false
		}
	}
	for id := range useIDs {
		if !resultIDs[id] {
			return false
		}
	}
	for index, message := range kept {
		if message.Role != "system" {
			continue
		}
		if index+1 < len(kept) && kept[index+1].Role != "assistant" {
			return false
		}
	}
	return true
}
