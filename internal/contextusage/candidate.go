package contextusage

import (
	"context"
	"sync"
)

// CandidateRequest is the typed input to CandidateProber.CountCandidate.
// The fields are the minimum needed to materialize a disposable session
// the provider can probe through its native /context view. CandidateJSONL
// is the full byte content of a single-message JSONL transcript that the
// provider will write to its session directory and resume against.
type CandidateRequest struct {
	// LiveSessionID is the session whose project directory and workspace
	// the candidate should be measured against. Required so providers
	// that resolve memory files, skills, and system prompt from the
	// workspace measure the candidate under the same conditions as the
	// live session.
	LiveSessionID string

	// TranscriptPath is the live session's on-disk transcript path. The
	// provider derives its project directory from this path so the
	// disposable candidate file lands in the same directory the live
	// session resides in. Required.
	TranscriptPath string

	// WorkDir is the cwd the provider should spawn against. Required
	// for memory files and skills to resolve identically to the live
	// session.
	WorkDir string

	// CandidateJSONL is the full byte content of a single-line JSONL
	// file representing the candidate transcript. The provider writes
	// this verbatim to a disposable session file, probes it, then
	// removes the file.
	CandidateJSONL []byte
}

// CandidateProber is the generic contract the compact planner relies on
// to project a candidate transcript's token count against the same
// counter the user will see on resume. Implementations live in provider
// packages and register themselves through RegisterCandidate at startup.
//
// CountCandidate must return the Messages-category token count for the
// candidate transcript as the provider's /context view would report it.
// Implementations are responsible for cleaning up any temporary session
// artifacts they materialize.
type CandidateProber interface {
	CountCandidate(ctx context.Context, req CandidateRequest) (int, error)
}

// candidateRegistry holds the provider-id -> CandidateProber mapping.
// Separate from the Prober registry because the two interfaces have
// different signatures and different lifetimes; a provider may register
// a Prober without yet supporting candidate counting.
var candidateRegistry sync.Map

// RegisterCandidate associates a provider id with its CandidateProber
// implementation. Callers in provider packages call this at startup. A
// subsequent RegisterCandidate for the same id replaces the previous
// entry, keeping test fakes simple.
func RegisterCandidate(providerID string, prober CandidateProber) {
	candidateRegistry.Store(providerID, prober)
}

// GetCandidate returns the CandidateProber registered for providerID, or
// false when no provider has registered. The compact planner calls this
// when it needs to project a candidate transcript's token count.
func GetCandidate(providerID string) (CandidateProber, bool) {
	value, ok := candidateRegistry.Load(providerID)
	if !ok {
		return nil, false
	}
	prober, ok := value.(CandidateProber)
	if !ok {
		return nil, false
	}
	return prober, true
}
