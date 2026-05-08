package sessionrename

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// PromptTemplate is the static prompt the worker sends to the LLM
// adapter route. The {redacted_content} marker is the only
// substitution point. The template explicitly forbids personal
// names, file paths, long numeric runs, and direct quotes, so the
// validator's reject rules align with what the prompt asks for.
//
// The marker uses single curly braces on purpose. Single braces
// avoid collisions with text/template's {{...}} syntax, so callers
// can substitute through a plain strings.Replace without spinning
// up a template engine.
const PromptTemplate = `You generate a short kebab-case label for a chat session.

Constraints:
- Output a single label, kebab-case, lowercase.
- Length 4 to 48 characters.
- Pattern: ^[a-z][a-z0-9-]+[a-z0-9]$.
- Do NOT include direct quotes from the input.
- Do NOT include personal names, project secrets, file paths, or numbers longer than 4 digits.
- Do NOT add commentary. Output only the label.

Conversation excerpt:

{redacted_content}

Label:`

// PromptMarker is the substitution point inside PromptTemplate.
// Callers replace this literal with the redacted content. The
// constant is exported so tests can assert binding without
// duplicating the literal.
const PromptMarker = "{redacted_content}"

// LLMCaller is the contract the worker uses to talk to the
// configured adapter route. PR4 owns the concrete implementation
// that resolves cfg.AutoName.Provider against the daemon adapter
// and honors the route's existing timeout. The interface here lets
// PR5's validation and prompt-binding logic test in isolation.
type LLMCaller interface {
	// Complete sends the assembled prompt to the configured
	// adapter route and returns the raw model output. The
	// implementation must honor ctx cancellation and the route's
	// timeout. The implementation must not retry on its own; the
	// worker owns retry policy.
	Complete(ctx context.Context, prompt string) (string, error)
}

// ErrEmptyResponse fires when the adapter returned a blank string
// after trimming. The worker treats this as a candidate-source
// failure and falls back to the transcript path.
var ErrEmptyResponse = errors.New("sessionrename: llm returned empty response")

// BuildPrompt substitutes the redacted content into the prompt
// template. The function uses a plain strings.Replace so the
// template stays free of text/template syntax. Callers should pass
// the output of RedactMessages directly.
func BuildPrompt(redacted string) string {
	return strings.Replace(PromptTemplate, PromptMarker, redacted, 1)
}

// CleanLLMOutput trims whitespace, strips a single pair of
// surrounding quotes if present, and lowercases the result. The
// helper exists so the LLM can return "Label-Like-This" or with
// trailing whitespace and the validator still gets a clean kebab
// candidate to inspect.
func CleanLLMOutput(raw string) string {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.Trim(cleaned, "\"'`")
	cleaned = strings.TrimSpace(cleaned)
	cleaned = strings.ToLower(cleaned)
	// The model occasionally returns a multi-line response. Take
	// the first non-empty line only, since the prompt asks for a
	// single label.
	for _, line := range strings.Split(cleaned, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return cleaned
}

// CandidateFromLLM runs the full LLM-backed candidate path: build
// the prompt, call the adapter, clean the output, and return the
// raw cleaned candidate. The caller validates the returned string
// through Validate. Errors from the adapter call propagate
// unchanged so the worker can log the underlying cause.
func CandidateFromLLM(ctx context.Context, caller LLMCaller, redacted string) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("sessionrename: llm caller is nil")
	}
	prompt := BuildPrompt(redacted)
	raw, err := caller.Complete(ctx, prompt)
	if err != nil {
		return "", fmt.Errorf("sessionrename: llm complete: %w", err)
	}
	cleaned := CleanLLMOutput(raw)
	if cleaned == "" {
		return "", ErrEmptyResponse
	}
	return cleaned, nil
}
