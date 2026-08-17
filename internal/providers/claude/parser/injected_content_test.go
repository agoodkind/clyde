package parser

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// writeInjectedFixture writes one transcript line whose user message carries
// the given content string and returns the file path.
func writeInjectedFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	line := `{"uuid":"1","type":"user","timestamp":"2026-08-02T10:00:00Z","message":{"role":"user","content":` + content + `}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func streamInjectedFixture(t *testing.T, path string, includeInjected bool) []string {
	t.Helper()
	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
		IncludeInjected:       includeInjected,
	}))
	if err != nil {
		t.Fatalf("collect messages: %v", err)
	}
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.Text)
	}
	return texts
}

// TestStreamStripsHookAdditionalContextAfterThePrompt covers the dominant
// injected shape: Claude Code splices the UserPromptSubmit hook output after
// the typed prompt as plain text with no wrapping tag. The typed prompt
// survives byte for byte and everything from the heading on is removed,
// because the harness always appends the block last.
func TestStreamStripsHookAdditionalContextAfterThePrompt(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"why is the build failing?\n\nUserPromptSubmit hook additional context: # Rules\n\nrespond in as few words as possible\n\nmore rule text"`)

	texts := streamInjectedFixture(t, path, false)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	if texts[0] != "why is the build failing?" {
		t.Fatalf("text=%q want the typed prompt alone", texts[0])
	}
}

// TestStreamStripsSessionStartHookContext covers the SessionStart variant of
// the same splice.
func TestStreamStripsSessionStartHookContext(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"continue the migration\n\nSessionStart hook additional context: injected instructions here"`)

	texts := streamInjectedFixture(t, path, false)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	if texts[0] != "continue the migration" {
		t.Fatalf("text=%q want the typed prompt alone", texts[0])
	}
}

// TestStreamKeepsHookContextWhenInjectedIsIncluded pins the knob: asking for
// injected content returns the message untouched, so an export that names the
// kind stays byte-faithful.
func TestStreamKeepsHookContextWhenInjectedIsIncluded(t *testing.T) {
	t.Parallel()
	body := "why is the build failing?\n\nUserPromptSubmit hook additional context: # Rules\n\nrule text"
	path := writeInjectedFixture(t,
		`"why is the build failing?\n\nUserPromptSubmit hook additional context: # Rules\n\nrule text"`)

	texts := streamInjectedFixture(t, path, true)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	if texts[0] != body {
		t.Fatalf("text=%q want the full message byte for byte", texts[0])
	}
}

// TestStreamDropsHookFeedbackLinesOnly pins the line-scoped rule for hook
// feedback: only the marked lines leave, because a Stop hook's feedback quotes
// the user's own goal text elsewhere in the message and a cut to end of
// message would delete it.
func TestStreamDropsHookFeedbackLinesOnly(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"first user line\nStop hook feedback:\n- the goal condition text\nlast user line"`)

	texts := streamInjectedFixture(t, path, false)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	want := "first user line\n- the goal condition text\nlast user line"
	if texts[0] != want {
		t.Fatalf("text=%q want %q", texts[0], want)
	}
}

// TestStreamKeepsAQuotedHookHeadingBeforeTheRealSplice pins the collision the
// review found: a person asking about the hook by name quotes the heading in
// their prompt, and the genuine splice still follows at the end. The cut
// anchors at the last line-start match, so the person's words, including the
// quoted heading, survive and only the trailing splice is removed.
func TestStreamKeepsAQuotedHookHeadingBeforeTheRealSplice(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"why does every message contain\nUserPromptSubmit hook additional context: markers?\nplease investigate\n\nUserPromptSubmit hook additional context: # Rules\n\nrule text"`)

	texts := streamInjectedFixture(t, path, false)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	want := "why does every message contain\nUserPromptSubmit hook additional context: markers?\nplease investigate"
	if texts[0] != want {
		t.Fatalf("text=%q want the typed prompt with its quoted heading kept", texts[0])
	}
}

// TestStreamKeepsAQuotedHeadingOfTheOtherType pins the cross-type collision:
// a person quotes one heading at line start and the genuine splice that
// follows is the OTHER heading type. The cut anchors at the last match across
// every type, so the person's text between the quote and the splice survives.
func TestStreamKeepsAQuotedHeadingOfTheOtherType(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"my logs show this line:\nUserPromptSubmit hook additional context: something\nis that expected?\n\nSessionStart hook additional context: injected text"`)

	texts := streamInjectedFixture(t, path, false)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	want := "my logs show this line:\nUserPromptSubmit hook additional context: something\nis that expected?"
	if texts[0] != want {
		t.Fatalf("text=%q want the typed prompt with its quoted heading kept", texts[0])
	}
}

// TestStreamKeepsAnInlineHeadingMention pins the line-anchor guard: a heading
// mentioned mid-line is a person's sentence, not a splice, and the message is
// untouched.
func TestStreamKeepsAnInlineHeadingMention(t *testing.T) {
	t.Parallel()
	body := "the string UserPromptSubmit hook additional context: appears in my logs, why?"
	path := writeInjectedFixture(t,
		`"the string UserPromptSubmit hook additional context: appears in my logs, why?"`)

	texts := streamInjectedFixture(t, path, false)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	if texts[0] != body {
		t.Fatalf("text=%q want the message untouched", texts[0])
	}
}

// TestStreamStripsLegacyHookCarrierTag covers the tag Claude Code used before
// it switched to splicing plain text.
func TestStreamStripsLegacyHookCarrierTag(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"real question\n<user-prompt-submit-hook>old style hook payload</user-prompt-submit-hook>"`)

	texts := streamInjectedFixture(t, path, false)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	if texts[0] != "real question" {
		t.Fatalf("text=%q want %q", texts[0], "real question")
	}
}

// TestStreamCountsWhatItStripped pins the observability contract: the load
// tally reports how many injected and system pieces the parse removed, so the
// feed can log that stripping ran without knowing any marker.
func TestStreamCountsWhatItStripped(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"typed prompt\n<system-reminder>reminder</system-reminder>\n\nUserPromptSubmit hook additional context: rules"`)

	var tally transcript.HarnessStrips
	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
		IncludeInjected:       false,
		HarnessTally:          &tally,
	}))
	if err != nil {
		t.Fatalf("collect messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages=%d want 1", len(messages))
	}
	if tally.Injected != 1 {
		t.Fatalf("injected tally=%d want 1", tally.Injected)
	}
	if tally.System != 1 {
		t.Fatalf("system tally=%d want 1", tally.System)
	}
}

// TestStreamTallyCountsFullyStrippedRecords pins the CLYDE-639 fix: a user
// record that is entirely harness content strips to empty and is dropped, and
// the tally still counts it. Per-message counting lost exactly these, which
// made the feed's strip counters read near zero while thousands of exclusions
// happened.
func TestStreamTallyCountsFullyStrippedRecords(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"<task-notification>agent finished</task-notification>"`)

	var tally transcript.HarnessStrips
	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
		IncludeInjected:       false,
		HarnessTally:          &tally,
	}))
	if err != nil {
		t.Fatalf("collect messages: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages=%d want 0: the record is entirely harness content", len(messages))
	}
	if tally.System != 1 {
		t.Fatalf("system tally=%d want 1: the dropped record must still count", tally.System)
	}
}

// TestStreamStripsBashInputTagPair covers the bash capture triplet's input tag,
// which the noise list previously lacked while stripping the stdout and stderr
// halves.
func TestStreamStripsBashInputTagPair(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t,
		`"<bash-input>make deploy</bash-input>\nplease look at the failure"`)

	texts := streamInjectedFixture(t, path, false)
	if len(texts) != 1 {
		t.Fatalf("messages=%d want 1", len(texts))
	}
	if texts[0] != "please look at the failure" {
		t.Fatalf("text=%q want %q", texts[0], "please look at the failure")
	}
}
