package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const markerCurl = `#!/usr/bin/env bash
printf called > "$CURL_MARKER"
`

const installerCurl = `#!/usr/bin/env bash
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$@" > "$INSTALL_ARGUMENTS_PATH"'
`

func TestInstallScriptRequiresExplicitSelectionBeforeCurl(t *testing.T) {
	repositoryRoot := installTestRepositoryRoot(t)
	fakeBin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "curl-called")
	writeExecutable(t, filepath.Join(fakeBin, "curl"), markerCurl)

	command := exec.Command("bash", filepath.Join(repositoryRoot, "install.sh"))
	command.Env = installTestEnvironment(fakeBin, "CURL_MARKER="+marker)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("install.sh without a selection succeeded")
	}
	if !strings.Contains(string(output), "usage:") {
		t.Fatalf("output missing help:\n%s", output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("curl ran before selection validation: %v", err)
	}
}

func TestInstallScriptPassesSelectedComponentsAfterDownload(t *testing.T) {
	repositoryRoot := installTestRepositoryRoot(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "daemon", args: []string{"--daemon"}, want: "--repo\nagoodkind/clyde\n--binary\nclyde\n--\ninstall\nsetup\n--daemon\n"},
		{name: "hooks and mcp", args: []string{"--hooks", "--mcp"}, want: "--repo\nagoodkind/clyde\n--binary\nclyde\n--\ninstall\nsetup\n--hooks\n--mcp\n"},
		{name: "binary only", args: []string{"--binary-only"}, want: "--repo\nagoodkind/clyde\n--binary\nclyde\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			argumentsPath := filepath.Join(t.TempDir(), "arguments")
			writeExecutable(t, filepath.Join(fakeBin, "curl"), installerCurl)

			commandArgs := append([]string{filepath.Join(repositoryRoot, "install.sh")}, test.args...)
			command := exec.Command("bash", commandArgs...)
			command.Env = installTestEnvironment(fakeBin, "INSTALL_ARGUMENTS_PATH="+argumentsPath)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("install.sh: %v\n%s", err, output)
			}
			body, err := os.ReadFile(argumentsPath)
			if err != nil {
				t.Fatalf("ReadFile arguments: %v", err)
			}
			if string(body) != test.want {
				t.Fatalf("arguments:\n%s\nwant:\n%s", body, test.want)
			}
		})
	}
}

func TestInstallScriptRejectsInvalidSelectionsBeforeCurl(t *testing.T) {
	repositoryRoot := installTestRepositoryRoot(t)
	for _, args := range [][]string{{"--unknown"}, {"--binary-only", "--mcp"}} {
		fakeBin := t.TempDir()
		marker := filepath.Join(t.TempDir(), "curl-called")
		writeExecutable(t, filepath.Join(fakeBin, "curl"), markerCurl)
		commandArgs := append([]string{filepath.Join(repositoryRoot, "install.sh")}, args...)
		command := exec.Command("bash", commandArgs...)
		command.Env = installTestEnvironment(fakeBin, "CURL_MARKER="+marker)
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("install.sh %v succeeded:\n%s", args, output)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("curl ran for %v: %v", args, err)
		}
	}
}

func installTestRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filename))
}

func installTestEnvironment(fakeBin string, values ...string) []string {
	environment := make([]string, 0, len(os.Environ())+1+len(values))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, "PATH="+fakeBin+":/usr/bin:/bin")
	environment = append(environment, values...)
	return environment
}

func writeExecutable(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
