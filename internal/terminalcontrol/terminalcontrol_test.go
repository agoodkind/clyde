package terminalcontrol

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteSequenceHandlesSuspendPrep(t *testing.T) {
	var out bytes.Buffer
	writeSequence(&out, SuspendPrepSequence)
	if got := out.String(); got != SuspendPrepSequence {
		t.Fatalf("prep sequence = %q, want %q", got, SuspendPrepSequence)
	}
}

func TestModeResetSequenceCleansInteractiveModes(t *testing.T) {
	for _, seq := range []string{
		"\x1b[?2004l",
		"\x1b[?1004l",
		"\x1b[?1000l",
		"\x1b[?1002l",
		"\x1b[?1003l",
		"\x1b[?1006l",
		"\x1b[?1049l",
		"\x1b[?25h",
		"\x1b[?7h",
		"\x1b[r",
		"\r",
	} {
		if !strings.Contains(ModeResetSequence, seq) {
			t.Fatalf("ModeResetSequence missing %q", seq)
		}
	}
}

func TestModeResetEnablesAutowrapAfterSoftReset(t *testing.T) {
	softReset := strings.LastIndex(ModeResetSequence, "\x1b[!p")
	autowrap := strings.LastIndex(ModeResetSequence, "\x1b[?7h")
	if softReset < 0 {
		t.Fatalf("ModeResetSequence missing soft reset")
	}
	if autowrap < 0 {
		t.Fatalf("ModeResetSequence missing autowrap enable")
	}
	if autowrap < softReset {
		t.Fatalf("autowrap enable must follow soft reset")
	}
}
