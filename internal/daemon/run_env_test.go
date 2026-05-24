package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadInheritedRuntimeAcceptsReadyFDWithoutListeners(t *testing.T) {
	readyFilePath := filepath.Join(t.TempDir(), "ready")
	readyFile, err := os.Create(readyFilePath)
	if err != nil {
		t.Fatalf("create ready file: %v", err)
	}
	defer readyFile.Close()

	t.Setenv(envDaemonInheritedListeners, "")
	t.Setenv(envDaemonReadyFD, "3")

	oldNewFile := osNewFile
	t.Cleanup(func() {
		osNewFile = oldNewFile
	})
	osNewFile = func(fd uintptr, name string) *os.File {
		if fd != 3 {
			t.Fatalf("fd = %d, want 3", fd)
		}
		if name != "daemon-ready" {
			t.Fatalf("name = %q, want daemon-ready", name)
		}
		return readyFile
	}

	inherited, err := loadInheritedRuntime()
	if err != nil {
		t.Fatalf("load inherited runtime: %v", err)
	}
	if inherited.ready != readyFile {
		t.Fatalf("ready file = %v, want injected file", inherited.ready)
	}
}
