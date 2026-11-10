package cursorjsonl

import "testing"

// TestIsInjectedUserPromptMatchesWhatCursorWrites uses the texts as they
// appear in real transcripts, after the envelope is unwrapped.
func TestIsInjectedUserPromptMatchesWhatCursorWrites(t *testing.T) {
	t.Parallel()
	stored := "<timestamp>Wednesday, Jul 29, 2026, 10:27 PM (UTC+2)</timestamp>\n\n" +
		"<user_query>Briefly inform the user about the task result and perform any follow-up actions (if needed).</user_query>"
	unwrapped, _ := UnwrapUserEnvelope(stored)
	if !IsInjectedUserPrompt(unwrapped) {
		t.Fatalf("unwrapped %q was not recognized as Cursor's own prompt", unwrapped)
	}
}

// TestIsInjectedUserPromptIgnoresTextStillInItsEnvelope states the ordering
// this depends on: the framing differs on every turn, so a caller that checks
// before unwrapping never matches and would silently keep every copy.
func TestIsInjectedUserPromptIgnoresTextStillInItsEnvelope(t *testing.T) {
	t.Parallel()
	stored := "<user_query>Briefly inform the user about the task result and perform any follow-up actions (if needed).</user_query>"
	if IsInjectedUserPrompt(stored) {
		t.Fatal("text still inside its envelope matched, so the check no longer depends on unwrapping first")
	}
}

// TestIsInjectedUserPromptKeepsWhatAPersonWrote is the failure that matters.
// Someone discussing these prompts, or quoting one inside a longer message,
// must keep every word.
func TestIsInjectedUserPromptKeepsWhatAPersonWrote(t *testing.T) {
	t.Parallel()
	written := []string{
		"Briefly inform the user about the task result and perform any follow-up actions (if needed). Also run the tests.",
		"why does cursor send \"Briefly inform the user about the task result and perform any follow-up actions (if needed).\" every time?",
		"briefly inform the user about the task result and perform any follow-up actions (if needed).",
		"Perform any necessary follow-up actions.",
		"",
	}
	for _, text := range written {
		if IsInjectedUserPrompt(text) {
			t.Fatalf("a message a person wrote was treated as Cursor's own: %q", text)
		}
	}
}
