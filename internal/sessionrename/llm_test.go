package sessionrename

import (
	"context"
	"errors"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/session"
)

// stubLLMCaller is a test double that records the prompt it received
// and returns a canned response. The test uses this to assert that
// the prompt template binds the redacted content and that the
// candidate path returns the validated name.
type stubLLMCaller struct {
	gotPrompt string
	response  string
	err       error
}

func (s *stubLLMCaller) Complete(_ context.Context, prompt string) (string, error) {
	s.gotPrompt = prompt
	return s.response, s.err
}

func TestBuildPromptBindsRedactedContent(t *testing.T) {
	redacted := "first turn\n---\nsecond turn"
	got := BuildPrompt(redacted)
	if !strings.Contains(got, redacted) {
		t.Fatalf("BuildPrompt: redacted content missing from output\n%s", got)
	}
	if strings.Contains(got, PromptMarker) {
		t.Fatalf("BuildPrompt: marker %q still present in output", PromptMarker)
	}
	if !strings.Contains(got, "kebab-case") {
		t.Fatalf("BuildPrompt: expected the constraint preamble, got %q", got)
	}
}

func TestBuildPromptDoesNotUseTemplateBraces(t *testing.T) {
	if strings.Contains(PromptTemplate, "{{") || strings.Contains(PromptTemplate, "}}") {
		t.Fatalf("PromptTemplate must avoid text/template double-brace syntax")
	}
}

func TestCleanLLMOutputTrimsAndLowercases(t *testing.T) {
	cases := map[string]string{
		"  Auth-Login-Flow  \n":   "auth-login-flow",
		"\"billing-overhaul\"":    "billing-overhaul",
		"`db-migration-fix`":      "db-migration-fix",
		"first-line\nsecond-line": "first-line",
	}
	for input, want := range cases {
		got := CleanLLMOutput(input)
		if got != want {
			t.Errorf("CleanLLMOutput(%q): got %q, want %q", input, got, want)
		}
	}
}

func TestCandidateFromLLMReturnsCleanedOutput(t *testing.T) {
	caller := &stubLLMCaller{response: "  Login-Flow-Setup  \n"}
	got, err := CandidateFromLLM(context.Background(), caller, "redacted body")
	if err != nil {
		t.Fatalf("CandidateFromLLM: unexpected err %v", err)
	}
	if got != "login-flow-setup" {
		t.Fatalf("CandidateFromLLM: got %q, want %q", got, "login-flow-setup")
	}
	if !strings.Contains(caller.gotPrompt, "redacted body") {
		t.Fatalf("CandidateFromLLM: caller did not see redacted content, prompt was %q", caller.gotPrompt)
	}
}

func TestCandidateFromLLMReportsEmptyResponse(t *testing.T) {
	caller := &stubLLMCaller{response: "   "}
	_, err := CandidateFromLLM(context.Background(), caller, "redacted")
	if !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("CandidateFromLLM empty: got %v, want %v", err, ErrEmptyResponse)
	}
}

func TestCandidateFromLLMPropagatesAdapterError(t *testing.T) {
	want := errors.New("adapter unavailable")
	caller := &stubLLMCaller{err: want}
	_, err := CandidateFromLLM(context.Background(), caller, "redacted")
	if !errors.Is(err, want) {
		t.Fatalf("CandidateFromLLM err: got %v, want %v wrapped", err, want)
	}
}

func TestCandidateLLMPathSucceeds(t *testing.T) {
	caller := &stubLLMCaller{response: "auth-login-flow"}
	got, err := Candidate(context.Background(), CandidateInput{
		Provider:          "claude",
		FirstUserMessages: []string{"help me debug auth", "the login flow breaks", "after refresh"},
		Redact:            DefaultRedactPolicy(),
		LLM:               caller,
	})
	if err != nil {
		t.Fatalf("Candidate llm: unexpected err %v", err)
	}
	if got.Name != "auth-login-flow" {
		t.Fatalf("Candidate llm name: got %q, want %q", got.Name, "auth-login-flow")
	}
	if got.Source != session.AutoNameSourceLLM {
		t.Fatalf("Candidate llm source: got %q, want %q", got.Source, session.AutoNameSourceLLM)
	}
	if !strings.Contains(caller.gotPrompt, "help me debug auth") {
		t.Fatalf("Candidate llm prompt: redacted content missing, got %q", caller.gotPrompt)
	}
}

func TestCandidateLLMRejectFallsBackToTranscript(t *testing.T) {
	// LLM returns a name that fails validation. The worker must
	// fall back to the transcript path so we still get a name.
	caller := &stubLLMCaller{response: "/path/looking/name"}
	got, err := Candidate(context.Background(), CandidateInput{
		Provider:          "claude",
		FirstUserMessages: []string{"help debug auth login flow", "second", "third"},
		Redact:            DefaultRedactPolicy(),
		LLM:               caller,
	})
	if err != nil {
		t.Fatalf("Candidate fallback: unexpected err %v", err)
	}
	if got.Source != session.AutoNameSourceTranscript {
		t.Fatalf("Candidate fallback source: got %q, want %q", got.Source, session.AutoNameSourceTranscript)
	}
	if got.Name == "" {
		t.Fatalf("Candidate fallback: name is empty")
	}
}

func TestCandidateTranscriptOnlyPath(t *testing.T) {
	got, err := Candidate(context.Background(), CandidateInput{
		Provider:          ProviderTranscript,
		FirstUserMessages: []string{"help debug auth login flow", "second", "third"},
		Redact:            DefaultRedactPolicy(),
	})
	if err != nil {
		t.Fatalf("Candidate transcript: unexpected err %v", err)
	}
	if got.Source != session.AutoNameSourceTranscript {
		t.Fatalf("Candidate transcript source: got %q, want %q", got.Source, session.AutoNameSourceTranscript)
	}
	if got.Name == "" {
		t.Fatalf("Candidate transcript: name is empty")
	}
}

func TestCandidateLLMNilCallerFallsBack(t *testing.T) {
	got, err := Candidate(context.Background(), CandidateInput{
		Provider:          "claude",
		FirstUserMessages: []string{"help debug auth login", "second", "third"},
		Redact:            DefaultRedactPolicy(),
		LLM:               nil,
	})
	if err != nil {
		t.Fatalf("Candidate nil caller fallback: unexpected err %v", err)
	}
	if got.Source != session.AutoNameSourceTranscript {
		t.Fatalf("Candidate nil caller: source got %q, want transcript", got.Source)
	}
}
