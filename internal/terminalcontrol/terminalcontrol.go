// Package terminalcontrol centralizes terminal recovery used when Clyde
// hands the terminal between tcell and provider CLIs.
package terminalcontrol

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"golang.org/x/term"
)

// SuspendPrepSequence clears visible TUI state before provider CLI handoff.
const SuspendPrepSequence = "\x1b[0m\x1b[?7h\x1b[r\x1b[2J\x1b[H\r"

// ModeResetSequence disables terminal modes that can outlive child processes.
const ModeResetSequence = "" +
	"\x1b[?2004l" + // disable bracketed paste
	"\x1b[?1004l" + // disable focus reporting
	"\x1b[?1000l" + // disable X10 mouse
	"\x1b[?1002l" + // disable cell-motion mouse
	"\x1b[?1003l" + // disable any-motion mouse
	"\x1b[?1006l" + // disable SGR mouse
	"\x1b[?1007l" + // disable alternate-scroll wheel translation
	"\x1b[?1049l" + // exit alt screen
	"\x1b[!p" + // soft reset before restoring wanted private modes
	"\x1b[?25h" + // show cursor
	"\x1b[?7h" + // re-enable autowrap after the soft reset
	"\x1b[r" + // reset scroll region to full screen
	"\x1b[0m" + // reset SGR attributes
	"\x1b>" + // keypad normal
	"\r"

// ProcessRestorer restores terminal modes after tcell or a child process.
type ProcessRestorer struct {
	file  *os.File
	fd    int
	state *term.State
	log   *slog.Logger
}

// CaptureProcessRestorer snapshots the controlling terminal, when available.
func CaptureProcessRestorer(log *slog.Logger) *ProcessRestorer {
	if log == nil {
		log = slog.Default()
	}
	file, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		log.Debug("terminal.restore.open_tty_failed",
			"component", "terminal",
			"err", err)
		return nil
	}
	fd, ok := fileDescriptor(file)
	if !ok {
		log.Debug("terminal.restore.invalid_fd",
			"component", "terminal",
			"fd", file.Fd())
		_ = file.Close()
		return nil
	}
	state, err := term.GetState(fd)
	if err != nil {
		log.Debug("terminal.restore.snapshot_failed",
			"component", "terminal",
			"err", err)
		_ = file.Close()
		return nil
	}
	return &ProcessRestorer{
		file:  file,
		fd:    fd,
		state: state,
		log:   log,
	}
}

// Restore returns terminal modes and private terminal flags to normal.
func (r *ProcessRestorer) Restore() {
	if r == nil {
		return
	}
	if r.state != nil {
		err := term.Restore(r.fd, r.state)
		if err != nil {
			r.log.Warn("terminal.restore.state_failed",
				"component", "terminal",
				"err", err)
		}
	}
	writeSequence(r.file, ModeResetSequence)
	if r.file != nil {
		_ = r.file.Close()
	}
}

// WriteSuspendPrepToTerminal writes [SuspendPrepSequence] to the terminal.
func WriteSuspendPrepToTerminal() {
	writeSequenceToTerminal(SuspendPrepSequence)
}

// WriteResetToTerminal writes [ModeResetSequence] to the terminal.
func WriteResetToTerminal() {
	writeSequenceToTerminal(ModeResetSequence)
}

func writeSequenceToTerminal(sequence string) {
	file, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err == nil {
		writeSequence(file, sequence)
		_ = file.Close()
		return
	}
	fd, ok := fileDescriptor(os.Stderr)
	if ok && term.IsTerminal(fd) {
		writeSequence(os.Stderr, sequence)
	}
}

func writeSequence(w io.Writer, sequence string) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprint(w, sequence)
}

func fileDescriptor(file *os.File) (int, bool) {
	if file == nil {
		return 0, false
	}
	fd := file.Fd()
	const maxInt = int(^uint(0) >> 1)
	if fd > uintptr(maxInt) {
		return 0, false
	}
	return int(fd), true
}
