package updatehandoff

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDeployPathExecsDaemonDeploySubprocess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clyde-candidate")
	script := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := DeployPath(context.Background(), path, &stdout, &stderr); err != nil {
		t.Fatalf("DeployPath: %v; stderr=%q", err, stderr.String())
	}

	if stdout.String() != "daemon deploy\n" {
		t.Fatalf("stdout = %q, want daemon deploy", stdout.String())
	}
}

func TestDeployPathRejectsEmptyInstalledPath(t *testing.T) {
	err := DeployPath(context.Background(), "", &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("DeployPath succeeded with empty installed path")
	}
}
