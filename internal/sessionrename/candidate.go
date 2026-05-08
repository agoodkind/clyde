package sessionrename

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/session"
)

// ProviderTranscript is the literal value of cfg.AutoName.Provider
// that opts into the transcript-only path. Any other value (the
// default included) routes through the LLM-backed source.
const ProviderTranscript = "transcript"

// CandidateInput bundles every piece of state the candidate path
// needs. PR4's worker assembles this struct from the daemon's
// session view and the live config snapshot; PR5 keeps the shape
// stable so the swap to LLM-backed is a single field change at
// the worker call site.
type CandidateInput struct {
	// Provider is the cfg.AutoName.Provider value at the time of
	// the trigger. The empty string and any value other than
	// ProviderTranscript routes through the LLM path so day-one
	// behavior matches the summary subsystem.
	Provider string
	// FirstUserMessages is the raw text of the first three user
	// messages, in order. The worker reads these from the
	// transcript ahead of the candidate call.
	FirstUserMessages []string
	// Redact is the operator-configured redaction policy. The
	// worker passes a fully populated struct; the candidate path
	// applies it to every byte the LLM sees.
	Redact RedactPolicy
	// LLM is the adapter-backed LLMCaller for the configured
	// route. The candidate path requires a non-nil caller when
	// Provider routes through LLM.
	LLM LLMCaller
	// ExistingNames lists every other session's name with owner
	// attribution. Validate uses this list for both the user-owned
	// reject rule and the suffix increment rule.
	ExistingNames []ExistingNameOwner
	// ProviderSessionID is the session's current provider session
	// id literal. Validate rejects a candidate that matches it.
	ProviderSessionID string
}

// CandidateResult records the outcome of a candidate call so the
// worker can attribute the source on the rename mutation.
type CandidateResult struct {
	// Name is the validated kebab-case name. Empty on error.
	Name string
	// Source records which path produced the name. The worker
	// writes this value into Metadata.AutoNameSource when it
	// applies the rename.
	Source session.AutoNameSource
}

// Candidate runs the full candidate pipeline for one session. The
// function dispatches on Provider, runs redaction, calls the
// configured source, and validates the result. On any failure in
// the LLM path, the function falls back to the transcript path so
// the worker still has a chance to apply a name.
//
// PR4's Worker.candidateName implementation should call this
// function instead of the placeholder transcript-only logic so the
// integration step is a single line swap.
func Candidate(ctx context.Context, input CandidateInput) (CandidateResult, error) {
	redacted := RedactMessages(input.FirstUserMessages, input.Redact)

	if input.Provider == ProviderTranscript {
		return transcriptCandidate(input)
	}

	llmResult, llmErr := llmCandidate(ctx, input, redacted)
	if llmErr == nil {
		return llmResult, nil
	}

	slog.WarnContext(ctx, "sessionrename.candidate.llm_failed_fallback", "err", llmErr)
	fallbackResult, fallbackErr := transcriptCandidate(input)
	if fallbackErr != nil {
		return CandidateResult{}, fallbackErr
	}
	return fallbackResult, nil
}

// llmCandidate runs the LLM-backed path: build the prompt, call the
// adapter, clean the output, validate. The function returns a
// CandidateResult with Source set to AutoNameSourceLLM on success.
func llmCandidate(ctx context.Context, input CandidateInput, redacted string) (CandidateResult, error) {
	if input.LLM == nil {
		return CandidateResult{}, fmt.Errorf("sessionrename: llm caller required for provider %q", input.Provider)
	}
	if redacted == "" {
		return CandidateResult{}, fmt.Errorf("sessionrename: redacted content is empty")
	}
	rawCandidate, err := CandidateFromLLM(ctx, input.LLM, redacted)
	if err != nil {
		return CandidateResult{}, err
	}
	accepted, validateErr := Validate(ValidationInput{
		Candidate:         rawCandidate,
		ExistingNames:     input.ExistingNames,
		ProviderSessionID: input.ProviderSessionID,
	})
	if validateErr != nil {
		return CandidateResult{}, validateErr
	}
	return CandidateResult{
		Name:   accepted,
		Source: session.AutoNameSourceLLM,
	}, nil
}

// transcriptCandidate runs the transcript-only path. The function
// derives a kebab-shaped candidate from the first redacted message
// and validates it. The path matches PR4's transcript-only behavior
// so the swap from PR4 to PR5 keeps the same fallback semantics.
func transcriptCandidate(input CandidateInput) (CandidateResult, error) {
	if len(input.FirstUserMessages) == 0 {
		return CandidateResult{}, fmt.Errorf("sessionrename: no user messages")
	}
	first := input.FirstUserMessages[0]
	redactedFirst := Redact(first, input.Redact)
	candidate := transcriptCandidateName(redactedFirst)
	if candidate == "" {
		return CandidateResult{}, fmt.Errorf("sessionrename: transcript candidate is empty")
	}
	accepted, validateErr := Validate(ValidationInput{
		Candidate:         candidate,
		ExistingNames:     input.ExistingNames,
		ProviderSessionID: input.ProviderSessionID,
	})
	if validateErr != nil {
		return CandidateResult{}, validateErr
	}
	return CandidateResult{
		Name:   accepted,
		Source: session.AutoNameSourceTranscript,
	}, nil
}

// transcriptCandidateName produces a kebab-cased candidate from one
// already-redacted message. The function lowercases, replaces
// non-alnum runs with single hyphens, trims leading and trailing
// hyphens, and caps at 48 characters. The result may still fail
// validation; the caller handles that case.
func transcriptCandidateName(message string) string {
	lowered := strings.ToLower(message)
	var builder strings.Builder
	prevHyphen := false
	for _, char := range lowered {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
			prevHyphen = false
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
			prevHyphen = false
		case prevHyphen:
			continue
		default:
			builder.WriteRune('-')
			prevHyphen = true
		}
	}
	candidate := strings.Trim(builder.String(), "-")
	if len(candidate) > 48 {
		candidate = strings.TrimRight(candidate[:48], "-")
	}
	return candidate
}
