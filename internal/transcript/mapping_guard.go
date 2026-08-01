package transcript

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"goodkind.io/clyde/internal/slogger"
)

// ContainMappingPanic turns a panic raised while one record is mapped into the
// skip every mapping site already reports for a record it cannot use. Defer it
// as the first statement of a mapping function:
//
//	func mapThing(in Thing) (Message, bool) {
//		defer ContainMappingPanic()
//
// It needs no arguments and no signature change. Go allocates and zeroes a
// function's result parameters on entry whether or not they are named, and a
// function whose panic a deferred call recovers returns whatever those
// parameters hold, which is still the zero value. So the mapping function
// returns a zero message and a false include, which is byte for byte what its
// package-local emptyMessage helper returns when it declines a record on
// purpose. Callers need no change either: every one of them skips the record
// when include is false and none reads the message in that case.
//
// One result is not identical to a deliberate skip. Claude's parseLine also
// returns the tool results a line carried, and it returns them even when it
// declines the turn, because a user entry holding only tool results still
// answers the assistant turn before it. A contained panic returns those nil, so
// that line's tool outputs are lost along with the line. That is the correct
// reading: the reader tripped part way through, so nothing it produced for that
// line can be trusted.
//
// Containment sits at the mapping function rather than at its caller because a
// provider yields messages through a push iterator. The producer owns the
// stack, so a panic that reaches the consumer has already killed the iterator
// and it cannot be resumed. Recovering there would lose every later message in
// the conversation, and a conversation that delivers nothing is never marked
// satisfied by the engine, so it stays on the needed list and the daemon reads
// and fails it again on every pass. Recovering here costs one record.
//
// The stack is the finding. It names the mapping function, its file, and the
// line that raised, so nothing else needs to be passed in to say where to look.
//
// Two constraints keep this cheap on a path that runs once per message across
// the whole corpus, and both are load bearing.
//
// Defer it from the mapping function, never from a loop in the caller. The
// compiler open-codes a defer only at loop depth one; inside a loop both the
// closure and the runtime defer record move to the heap, which on a corpus pass
// of six hundred thousand messages is tens of megabytes of garbage per pass and
// costs several times what this shape costs.
//
// Take no arguments. A deferred call with arguments is rewritten into a
// generated wrapper closure, adding a call frame and a spill per message. With
// none, the call is already in normal form. The measured cost of the current
// shape is under one part in six thousand of a pass.
//
// A second defer in one of these functions would silently undo the first
// constraint. Open coding requires returns times defers to stay at fifteen or
// under, and the mapping functions already return from up to eleven places.
//
// Keep the results unnamed. The zero-result guarantee holds because a result
// parameter can only hold a value some statement put there, and a function with
// unnamed results assigns them once, at the return, after every expression in
// that return has already been evaluated. Naming them lets a function assign
// one early and then panic, and the guard would report that half-built value
// with include still true if the include result had been set. All six mapping
// functions leave their results unnamed today.
func ContainMappingPanic() {
	recovered := recover()
	if recovered == nil {
		return
	}
	slog.Error("transcript.mapping_panic",
		"concern", slogger.ConcernTranscript,
		"err", fmt.Sprintf("panic: %v", recovered),
		"stack", string(debug.Stack()),
	)
}
