package hookspec

import "testing"

func TestShellCommandQuotesArguments(t *testing.T) {
	t.Parallel()

	got := shellCommand("/tmp/clyde path", []string{"hooks", "run", "value with space"})
	want := "'/tmp/clyde path' hooks run 'value with space'"
	if got != want {
		t.Fatalf("shellCommand() = %q, want %q", got, want)
	}
}

func TestShellQuoteLeavesBareWordsUnchanged(t *testing.T) {
	t.Parallel()

	if got := shellQuote("/tmp/clyde"); got != "/tmp/clyde" {
		t.Fatalf("shellQuote() = %q, want /tmp/clyde", got)
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	t.Parallel()

	got := shellQuote("it's")
	want := "'it'\\''s'"
	if got != want {
		t.Fatalf("shellQuote() = %q, want %q", got, want)
	}
}

func TestShellQuoteQuotesEmptyString(t *testing.T) {
	t.Parallel()

	if got := shellQuote(""); got != "''" {
		t.Fatalf("shellQuote() = %q, want ''", got)
	}
	if isShellBareWord("") {
		t.Fatal("isShellBareWord(\"\") = true, want false")
	}
}
