package parser

import (
	"strings"
	"time"

	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

// chronologyLinkHops limits how many adjacent native sequences one anchor search
// may cross.
const chronologyLinkHops = 8

// chronologyKey matches messages by role and trimmed text.
type chronologyKey struct {
	role string
	text string
}

// chronologyCandidate pairs a bubble's creation time with its request ID.
type chronologyCandidate struct {
	created   time.Time
	requestID string
}

// bubbleChronologyKey returns the role and trimmed text for a user or assistant bubble.
// It rejects other bubble types and empty text.
func bubbleChronologyKey(bubble cursorstore.Bubble) (chronologyKey, bool) {
	var role string
	switch bubble.Type {
	case cursorstore.BubbleTypeUser:
		role = "user"
	case cursorstore.BubbleTypeAssistant:
		role = "assistant"
	default:
		return chronologyKey{role: "", text: ""}, false
	}
	text := strings.TrimSpace(bubble.Text)
	if text == "" {
		return chronologyKey{role: "", text: ""}, false
	}
	return chronologyKey{role: role, text: text}, true
}

// nativeChronologyKeys collects role and text pairs from available native user
// and assistant messages with nonempty text.
func nativeChronologyKeys(native cursorstore.NativeToolCalls) map[chronologyKey]bool {
	keys := make(map[chronologyKey]bool)
	for _, message := range native.Messages {
		if key, ok := nativeChronologyKey(message); ok {
			keys[key] = true
		}
	}
	return keys
}

func nativeChronologyKey(message cursorstore.NativeMessage) (chronologyKey, bool) {
	if message.Missing || message.Text == "" || (message.Role != "user" && message.Role != "assistant") {
		return chronologyKey{role: "", text: ""}, false
	}
	return chronologyKey{role: message.Role, text: message.Text}, true
}

// nativeChronologyTimes selects dates between tool anchors in native order.
// Repeated role and text pairs keep a date only when every occurrence agrees.
// It returns nil when the native messages do not provide a dated tool anchor.
func nativeChronologyTimes(native cursorstore.NativeToolCalls, callDates map[string][]time.Time, candidates map[chronologyKey][]chronologyCandidate) map[chronologyKey]time.Time {
	resolver := chronologyResolver{messages: native.Messages, callDates: callDates}
	if !resolver.hasAnchor() {
		return nil
	}
	results := make(map[chronologyKey][]time.Time)
	for position, message := range native.Messages {
		key, ok := nativeChronologyKey(message)
		if !ok {
			continue
		}
		results[key] = append(results[key], resolver.occurrenceTime(position, candidates[key]))
	}
	governed := make(map[chronologyKey]time.Time, len(results))
	for key, times := range results {
		chosen := times[0]
		for _, other := range times[1:] {
			if !other.Equal(chosen) {
				chosen = time.Time{}
				break
			}
		}
		governed[key] = chosen
	}
	return governed
}

// chronologyResolver searches native message sequences for dated tool calls.
type chronologyResolver struct {
	messages  []cursorstore.NativeMessage
	callDates map[string][]time.Time
}

func (resolver chronologyResolver) hasAnchor() bool {
	for _, message := range resolver.messages {
		if len(resolver.anchorTimes(message)) > 0 {
			return true
		}
	}
	return false
}

// anchorTimes returns nonzero dates for calls with agreeing bubble dates.
// It skips missing and duplicate native messages.
func (resolver chronologyResolver) anchorTimes(message cursorstore.NativeMessage) []time.Time {
	if message.Missing || message.Duplicate {
		return nil
	}
	times := make([]time.Time, 0, len(message.Calls))
	for _, call := range message.Calls {
		dates := resolver.callDates[call.ID]
		if len(dates) == 0 || dates[0].IsZero() {
			continue
		}
		consistent := true
		for _, other := range dates[1:] {
			if !other.Equal(dates[0]) {
				consistent = false
				break
			}
		}
		if consistent {
			times = append(times, dates[0])
		}
	}
	return times
}

// occurrenceTime selects one distinct candidate date inside inclusive tool bounds.
// It rejects unequal request IDs when both messages provide one.
// It returns zero unless both bounds exist and exactly one distinct date qualifies.
func (resolver chronologyResolver) occurrenceTime(position int, candidates []chronologyCandidate) time.Time {
	lower, lowerFound := resolver.search(position, -1)
	upper, upperFound := resolver.search(position, 1)
	if !lowerFound || !upperFound {
		return time.Time{}
	}
	requestID := resolver.messages[position].RequestID
	var chosen time.Time
	for _, candidate := range candidates {
		if candidate.created.IsZero() || (requestID != "" && candidate.requestID != "" && candidate.requestID != requestID) {
			continue
		}
		if candidate.created.Before(lower) || candidate.created.After(upper) {
			continue
		}
		if !chosen.IsZero() && !chosen.Equal(candidate.created) {
			return time.Time{}
		}
		chosen = candidate.created
	}
	return chosen
}

func (resolver chronologyResolver) sameSequence(left int, right int) bool {
	return resolver.messages[left].Source == resolver.messages[right].Source && resolver.messages[left].Archive == resolver.messages[right].Archive
}

func (resolver chronologyResolver) bounds(position int) (int, int) {
	first := position
	for first > 0 && resolver.sameSequence(first-1, position) {
		first--
	}
	last := position
	for last+1 < len(resolver.messages) && resolver.sameSequence(last+1, position) {
		last++
	}
	return first, last
}

// search finds the nearest dated tool message in the requested direction.
// It selects that message's latest call date backward or earliest call date forward.
// It crosses at most chronologyLinkHops adjacent sequences through matching users.
func (resolver chronologyResolver) search(position int, step int) (time.Time, bool) {
	current := position
	first, last := resolver.bounds(position)
	for hops := 0; ; hops++ {
		for cursor := current + step; cursor >= first && cursor <= last; cursor += step {
			times := resolver.anchorTimes(resolver.messages[cursor])
			if len(times) == 0 {
				continue
			}
			chosen := times[0]
			for _, other := range times[1:] {
				if (step < 0 && other.After(chosen)) || (step > 0 && other.Before(chosen)) {
					chosen = other
				}
			}
			return chosen, true
		}
		if hops >= chronologyLinkHops {
			return time.Time{}, false
		}
		shared, ok := resolver.boundaryUser(current, first, last, step)
		if !ok {
			return time.Time{}, false
		}
		neighbor := first - 1
		if step > 0 {
			neighbor = last + 1
		}
		if neighbor < 0 || neighbor >= len(resolver.messages) {
			return time.Time{}, false
		}
		nextFirst, nextLast := resolver.bounds(neighbor)
		linked, ok := resolver.linkedUser(shared, nextFirst, nextLast)
		if !ok {
			return time.Time{}, false
		}
		current, first, last = linked, nextFirst, nextLast
	}
}

// boundaryUser selects the first user before current when searching backward,
// or the last user after current when searching forward, skipping missing messages
// and empty request IDs. It requires the request ID to occur once among available
// users in the sequence.
func (resolver chronologyResolver) boundaryUser(current int, first int, last int, step int) (int, bool) {
	shared := -1
	if step < 0 {
		for index := first; index < current; index++ {
			if resolver.requestUser(index) {
				shared = index
				break
			}
		}
	} else {
		for index := last; index > current; index-- {
			if resolver.requestUser(index) {
				shared = index
				break
			}
		}
	}
	if shared < 0 {
		return 0, false
	}
	count := 0
	for index := first; index <= last; index++ {
		if resolver.requestUser(index) && resolver.messages[index].RequestID == resolver.messages[shared].RequestID {
			count++
		}
	}
	return shared, count == 1
}

func (resolver chronologyResolver) requestUser(index int) bool {
	message := resolver.messages[index]
	return !message.Missing && message.Role == "user" && message.RequestID != ""
}

// linkedUser finds a user in the adjacent sequence only when exactly one
// available user matches the shared user's request ID and content hash. It does
// not find a user when the shared user's content hash is empty.
func (resolver chronologyResolver) linkedUser(shared int, first int, last int) (int, bool) {
	if resolver.messages[shared].ContentHash == "" {
		return 0, false
	}
	linked := -1
	for index := first; index <= last; index++ {
		message := resolver.messages[index]
		if !resolver.requestUser(index) || message.RequestID != resolver.messages[shared].RequestID || message.ContentHash != resolver.messages[shared].ContentHash {
			continue
		}
		if linked >= 0 {
			return 0, false
		}
		linked = index
	}
	return linked, linked >= 0
}
