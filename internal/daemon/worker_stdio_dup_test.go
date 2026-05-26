//go:build unix

package daemon

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// dupFD returns a new os.File wrapping a dup of the given fd. The
// returned file is the caller's responsibility to close; it exists so
// tests can checkpoint the real fd 1 and fd 2 before
// RedirectWorkerStdio replaces them and restore the original handles
// afterward.
func dupFD(fd int) (*os.File, error) {
	newFD, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(newFD), ""), nil
}

// restoreFD points the given fd back at the file's underlying
// descriptor. The original file handle is closed so the descriptor
// table holds exactly one reference per fd after the call.
func restoreFD(t *testing.T, original *os.File, fd int) {
	t.Helper()
	if original == nil {
		return
	}
	if err := unix.Dup2(int(original.Fd()), fd); err != nil {
		t.Errorf("restore fd %d: %v", fd, err)
	}
	_ = original.Close()
}
