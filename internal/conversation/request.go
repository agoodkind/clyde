package conversation

import (
	"context"
	"strings"

	"goodkind.io/clyde/internal/providerid"
)

// RequestOrigin names which path answered a request-id lookup. The two paths
// read different stores and can disagree, so every answer carries the origin
// that produced it.
type RequestOrigin uint8

const (
	// RequestOriginUnspecified is the zero value, used when no path answered.
	RequestOriginUnspecified RequestOrigin = iota
	// RequestOriginIndex marks an answer that came from Clyde's own conversation
	// index, which is derived from the provider artifacts Clyde already scans.
	RequestOriginIndex
	// RequestOriginLive marks an answer that came from a bounded, indexed lookup
	// against the provider's own live store.
	RequestOriginLive
	// RequestOriginFullScan marks an answer that came from the opt-in exhaustive
	// scan of the provider's store.
	RequestOriginFullScan
)

// String returns the stable text form used on the wire and in output.
func (origin RequestOrigin) String() string {
	switch origin {
	case RequestOriginUnspecified:
		return "unspecified"
	case RequestOriginIndex:
		return "index"
	case RequestOriginLive:
		return "live"
	case RequestOriginFullScan:
		return "full_scan"
	default:
		return "unspecified"
	}
}

// Valid reports whether origin names a path that actually answered.
func (origin RequestOrigin) Valid() bool {
	switch origin {
	case RequestOriginUnspecified:
		return false
	case RequestOriginIndex, RequestOriginLive, RequestOriginFullScan:
		return true
	default:
		return false
	}
}

// RequestNotFoundReason explains why a request id resolved to nothing, for the
// cases where the reason is knowable. It is never a guess: an unresolvable id
// reports not found rather than a nearby conversation.
type RequestNotFoundReason uint8

const (
	// RequestNotFoundReasonUnspecified is the zero value, used when the lookup
	// succeeded or when no path could say why it failed.
	RequestNotFoundReasonUnspecified RequestNotFoundReason = iota
	// RequestNotFoundReasonNoResolver marks a corpus where no registered provider
	// can resolve request ids at all.
	RequestNotFoundReasonNoResolver
	// RequestNotFoundReasonNotRetained marks an id the provider no longer lists
	// among its recent requests, which is what an id older than the provider's
	// retention window looks like.
	RequestNotFoundReasonNotRetained
	// RequestNotFoundReasonNoMatchingConversation marks an id the provider still
	// lists but that no conversation in the searched set carries.
	RequestNotFoundReasonNoMatchingConversation
	// RequestNotFoundReasonUnindexedConversation marks an id that resolved to a
	// provider conversation Clyde's index does not hold.
	RequestNotFoundReasonUnindexedConversation
	// RequestNotFoundReasonAmbiguousConversation marks an id that several
	// conversations carry, which no lookup can narrow to one without guessing.
	// Duplicating a conversation duplicates the turns inside it, request ids
	// included, and the copies are indistinguishable from the original by the id
	// alone.
	RequestNotFoundReasonAmbiguousConversation
	// RequestNotFoundReasonInconclusive marks a lookup that could not read part of
	// the provider's store, so the miss proves nothing. The provider application
	// runs while Clyde reads, and a store it has locked reads exactly like one
	// that never held the id.
	RequestNotFoundReasonInconclusive
)

// String returns the stable text form used on the wire and in output.
func (reason RequestNotFoundReason) String() string {
	switch reason {
	case RequestNotFoundReasonUnspecified:
		return "unspecified"
	case RequestNotFoundReasonNoResolver:
		return "no_resolver"
	case RequestNotFoundReasonNotRetained:
		return "not_retained"
	case RequestNotFoundReasonNoMatchingConversation:
		return "no_matching_conversation"
	case RequestNotFoundReasonUnindexedConversation:
		return "unindexed_conversation"
	case RequestNotFoundReasonAmbiguousConversation:
		return "ambiguous_conversation"
	case RequestNotFoundReasonInconclusive:
		return "inconclusive"
	default:
		return "unspecified"
	}
}

// Describe returns the operator-facing sentence for the reason.
func (reason RequestNotFoundReason) Describe() string {
	switch reason {
	case RequestNotFoundReasonUnspecified:
		return "no path could resolve the request id"
	case RequestNotFoundReasonNoResolver:
		return "no registered provider resolves request ids"
	case RequestNotFoundReasonNotRetained:
		return "the provider does not list this request among its recent requests, so it is either older than the provider's retention window or was never issued on this machine"
	case RequestNotFoundReasonNoMatchingConversation:
		return "the provider still lists this request but no conversation in the searched set carries it"
	case RequestNotFoundReasonUnindexedConversation:
		return "the request belongs to a conversation Clyde's index does not hold"
	case RequestNotFoundReasonAmbiguousConversation:
		return "several conversations carry this request, so no one of them is the answer; duplicating a conversation copies the turns inside it, request ids included"
	case RequestNotFoundReasonInconclusive:
		return "part of the provider's store could not be read, so this miss proves nothing; retry once the provider is idle"
	default:
		return "no path could resolve the request id"
	}
}

// RequestLookupOptions configures one request-id lookup.
type RequestLookupOptions struct {
	// AllowFullScan permits the exhaustive provider-store scan after the bounded
	// paths miss. It costs tens of seconds, so only a caller that asked for it
	// knowingly sets it, and no code path enables it on its own.
	AllowFullScan bool
}

// MergeRequestNotFoundReason folds one lookup's reason into the reason reported
// so far, and is the one place that precedence is written.
//
// Ambiguity outranks everything. A store that found several carriers established
// that the request exists and that no one conversation answers for it, and no
// other store can weaken that: one finding nothing does not make the carriers
// disappear, and one being unreadable does not make the question answerable.
// Without this, two Cursor installs where the first has no ring entry and the
// second holds the request in two chats report not-retained, whose text says the
// id was never issued on this machine, which the second root just disproved.
//
// An inconclusive result outranks every confirmed one. A lookup that spans
// several stores, roots, or providers cannot report a confirmed absence when any
// part of what it searched could not be read, because the part it could not read
// is exactly where the answer might have been.
//
// Otherwise the first knowable reason stands, since a later store having nothing
// to say does not weaken what an earlier one established.
func MergeRequestNotFoundReason(soFar RequestNotFoundReason, next RequestNotFoundReason) RequestNotFoundReason {
	for _, outranking := range []RequestNotFoundReason{
		RequestNotFoundReasonAmbiguousConversation,
		RequestNotFoundReasonInconclusive,
	} {
		if soFar == outranking {
			return soFar
		}
		if next == outranking {
			return next
		}
	}
	if soFar == RequestNotFoundReasonUnspecified {
		return next
	}
	return soFar
}

// RequestMatch is what one provider's resolver reports for a request id. Found
// discriminates the two shapes: a match carries the provider, the native
// conversation id, and the origin that produced it, and a miss carries the reason
// when one is knowable.
type RequestMatch struct {
	Found bool
	// Provider names the store that answered. It travels with the native id
	// because a native conversation id is only unique within its own provider, so
	// mapping one back to an index record without it can answer with another
	// provider's conversation.
	Provider             providerid.Provider
	NativeConversationID string
	Origin               RequestOrigin
	Reason               RequestNotFoundReason
}

// RequestResolver is the optional capability of a provider [Parser] that can map
// one of its native request ids to the conversation that issued it. A provider
// whose artifacts carry no request id simply does not implement it.
//
// The resolver must not run an unbounded scan unless opts.AllowFullScan is set,
// and it must never return a nearby or guessed conversation.
type RequestResolver interface {
	ResolveRequestID(ctx context.Context, requestID string, opts RequestLookupOptions) (RequestMatch, error)
}

// RequestResolution is the index-level answer for one request id: whether it
// resolved, which path answered, the conversation it belongs to, and, when it
// did not resolve, why.
type RequestResolution struct {
	RequestID string
	Found     bool
	Origin    RequestOrigin
	Reason    RequestNotFoundReason
	Record    Record
}

// requestIDLength is the length of the dash-separated UUID form every provider
// request id Clyde resolves is written in.
const requestIDLength = 36

// looksLikeRequestID reports whether a selector is shaped like a provider
// request id. Resolve consults it before reaching for a provider's live store,
// so an ordinary missing title never triggers a store lookup. It is a check on
// the input's shape, not on any failure the lookup produced.
func looksLikeRequestID(selector string) bool {
	if len(selector) != requestIDLength {
		return false
	}
	for index, char := range selector {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !isHexDigit(char) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(char rune) bool {
	if char >= '0' && char <= '9' {
		return true
	}
	if char >= 'a' && char <= 'f' {
		return true
	}
	return char >= 'A' && char <= 'F'
}

// normalizeRequestID trims a caller-supplied request id to the exact string the
// provider stores. Matching stays whole-string: nothing is lowercased or
// truncated, so a value that merely contains the id never matches.
func normalizeRequestID(requestID string) string {
	return strings.TrimSpace(requestID)
}
