package daemon

import (
	"testing"
)

func TestReplacementDaemonStarterForPlatformUsesSupervisorOnDarwinWithSocket(t *testing.T) {
	starter := replacementDaemonStarterForPlatform("darwin", "/tmp/clyde-supervisor.sock")
	if _, ok := starter.(supervisorReplacementDaemonStarter); !ok {
		t.Fatalf("starter type = %T, want supervisorReplacementDaemonStarter", starter)
	}
}

func TestReplacementDaemonStarterForPlatformUsesRuntimeSupervisorSocketOnDarwinWithoutEnv(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	starter := replacementDaemonStarterForPlatform("darwin", "")
	got, ok := starter.(supervisorReplacementDaemonStarter)
	if !ok {
		t.Fatalf("starter type = %T, want supervisorReplacementDaemonStarter", starter)
	}
	if got.socketPath != supervisorSocketPath() {
		t.Fatalf("socket path = %q, want %q", got.socketPath, supervisorSocketPath())
	}
}
