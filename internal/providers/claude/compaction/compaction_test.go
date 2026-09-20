package compaction

import (
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation/exportargs"
)

// compactionPrompt builds the prompt Claude Code sends, with instructions the
// operator typed after /compact.
func compactionPrompt(instructions string) string {
	prompt := "Your task is to create a detailed summary of the conversation so far."
	if instructions != "" {
		prompt += "\n\nAdditional Instructions:\n" + instructions
	}
	return prompt + "\n\nREMINDER: Do NOT call any tools. Respond with plain text only."
}

func TestPromptIndexFindsThePromptBehindASystemReminder(t *testing.T) {
	t.Parallel()
	roles := []string{"user", "assistant", "user", "system"}
	texts := []string{"start", "reply", compactionPrompt(""), "<system-reminder>"}
	index, ok := PromptIndex(roles, texts)
	if !ok {
		t.Fatal("PromptIndex found no compaction prompt")
	}
	if index != 2 {
		t.Fatalf("index = %d, want 2", index)
	}
}

func TestPromptIndexRejectsANormalTurn(t *testing.T) {
	t.Parallel()
	roles := []string{"user", "assistant", "user"}
	texts := []string{"start", "reply", "summarize the tests for me"}
	if _, ok := PromptIndex(roles, texts); ok {
		t.Fatal("a normal turn matched the compaction prompt")
	}
}

func TestArgumentsReadsEveryDeclaredArgument(t *testing.T) {
	t.Parallel()
	for _, declaration := range exportargs.Declarations() {
		flag := "--" + strings.ReplaceAll(declaration.Canonical, "_", "-")
		typed := flag
		if declaration.Kind != exportargs.KindBool {
			typed += " " + argumentValue(declaration)
		}
		args := Arguments(compactionPrompt(typed))
		if len(args) == 0 {
			t.Errorf("argument %q read nothing from %q", declaration.Canonical, typed)
			continue
		}
		if args[0] != flag {
			t.Errorf("argument %q read %v", declaration.Canonical, args)
		}
	}
}

// argumentValue returns a value the declaration accepts.
func argumentValue(declaration exportargs.Declaration) string {
	if len(declaration.Values) > 0 {
		return declaration.Values[0]
	}
	if declaration.Kind == exportargs.KindInt {
		return "0"
	}
	return "20k"
}

func TestArgumentsStopsAtProse(t *testing.T) {
	t.Parallel()
	args := Arguments(compactionPrompt("--max-tokens 50k focus on the test failures"))
	want := []string{"--max-tokens", "50k"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestArgumentsReadsNothingFromProseAlone(t *testing.T) {
	t.Parallel()
	if args := Arguments(compactionPrompt("focus on the test failures")); len(args) != 0 {
		t.Fatalf("args = %v, want none", args)
	}
}

func TestArgumentsReadsNothingWithoutAnInstructionBlock(t *testing.T) {
	t.Parallel()
	if args := Arguments(compactionPrompt("")); len(args) != 0 {
		t.Fatalf("args = %v, want none", args)
	}
}
