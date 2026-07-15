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

func TestCommandHasManagedArgsMatchesSpacedReorientSyntax(t *testing.T) {
	t.Parallel()

	command := "/usr/local/bin/clyde hooks run reorient before-compact"
	signatures := [][]string{{"hooks", "run", "reorient", "before-compact"}}
	if !commandHasManagedArgs(command, signatures) {
		t.Fatalf("commandHasManagedArgs(%q) = false, want true", command)
	}
}

func TestCommandHasManagedArgsMatchesLegacySessionStartCommand(t *testing.T) {
	t.Parallel()

	command := "/opt/old/clyde hook sessionstart"
	signatures := [][]string{{"hook", "sessionstart"}}
	if !commandHasManagedArgs(command, signatures) {
		t.Fatalf("commandHasManagedArgs(%q) = false, want true", command)
	}
}
