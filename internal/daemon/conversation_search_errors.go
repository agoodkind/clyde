package daemon

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// semanticEngineUnavailableError signals that conversation semantic search is
// configured but the engine is unreachable: registration has not succeeded, or
// an engine call failed with a transport/unavailable error. The query path
// returns it fast instead of running an unbounded full-corpus literal scan, and
// the RPC boundary maps it to codes.FailedPrecondition so the caller sees a
// typed dependency failure rather than a hang.
type semanticEngineUnavailableError struct {
	operation semanticEngineOperation
	err       error
}

// semanticEngineOperation names the step at which the engine was found
// unreachable, so the returned error says whether the daemon never registered
// the collection or lost the engine mid-query.
type semanticEngineOperation string

const (
	// semanticEngineOperationRegistration means the daemon has no registered
	// engine connection yet: the boot-time dial or register failed and the
	// background retry has not succeeded.
	semanticEngineOperationRegistration semanticEngineOperation = "registration"
	// semanticEngineOperationSearch means a registered connection failed the
	// search call with a transport error.
	semanticEngineOperationSearch semanticEngineOperation = "search"
)

func (e semanticEngineUnavailableError) Error() string {
	message := "conversation semantic engine unavailable during " + string(e.operation)
	if e.err != nil {
		return message + ": " + e.err.Error()
	}
	return message
}

func (e semanticEngineUnavailableError) Unwrap() error {
	return e.err
}

// semanticEngineSearchError preserves a non-transport engine rejection as a
// failed search. The RPC boundary uses the engine's typed gRPC code and message
// so callers receive the actionable refusal instead of an empty result.
type semanticEngineSearchError struct {
	err error
}

func (e semanticEngineSearchError) Error() string {
	return "conversation semantic engine search failed: " + e.err.Error()
}

func (e semanticEngineSearchError) Unwrap() error {
	return e.err
}

func (e semanticEngineSearchError) rpcCode() codes.Code {
	var statusErr grpcStatusError
	if errors.As(e.err, &statusErr) {
		return statusErr.GRPCStatus().Code()
	}
	return codes.Internal
}

// grpcStatusError is the interface a gRPC status error implements. Matching it
// through [errors.As] walks the wrapped chain, so a transport failure is still
// classified after a caller wraps it.
type grpcStatusError interface {
	error
	GRPCStatus() *status.Status
}

// isEngineUnavailable reports whether err (or any error it wraps) is a gRPC
// status naming an unreachable engine: Unavailable (connection refused, engine
// down) or DeadlineExceeded (engine wedged).
func isEngineUnavailable(err error) bool {
	var statusErr grpcStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	code := statusErr.GRPCStatus().Code()
	return code == codes.Unavailable || code == codes.DeadlineExceeded
}
