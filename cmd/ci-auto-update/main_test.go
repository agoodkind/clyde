package main

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/go-makefile/selfupdate"
)

func TestSelectReleasesForBranchBuild(t *testing.T) {
	t.Parallel()
	releases := []githubRelease{
		{TagName: "202608121600-13b-abcdef12"},
		{TagName: "202608111500-13a-12345678"},
	}
	env := environment{
		commit:  "abcdef1234567890",
		refType: "branch",
		refName: "main",
	}

	selection, err := selectReleases(releases, env)
	if err != nil {
		t.Fatalf("selectReleases() error = %v", err)
	}
	if selection.target != releases[0].TagName || selection.previous != releases[1].TagName {
		t.Fatalf("selection = %#v, want target %q and previous %q", selection, releases[0].TagName, releases[1].TagName)
	}
}

func TestSelectReleasesForManualRunUsesLatestRelease(t *testing.T) {
	t.Parallel()
	releases := []githubRelease{
		{TagName: "202608121600-13b-abcdef12"},
		{TagName: "202608111500-13a-12345678"},
	}
	env := environment{
		commit:  "unreleased123456",
		refType: "branch",
		refName: "main",
		manual:  true,
	}

	selection, err := selectReleases(releases, env)
	if err != nil {
		t.Fatalf("selectReleases() error = %v", err)
	}
	if selection.target != releases[0].TagName || selection.previous != releases[1].TagName {
		t.Fatalf("selection = %#v, want latest two releases", selection)
	}
}

func TestLoadEnvironmentDetectsManualRun(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"GITHUB_REPOSITORY": "agoodkind/clyde",
		"GITHUB_SHA":        "abcdef1234567890",
		"GITHUB_REF_TYPE":   "branch",
		"GITHUB_REF_NAME":   "main",
		"GITHUB_EVENT_NAME": "workflow_dispatch",
		"GH_TOKEN":          "token",
	}

	env, err := loadEnvironment(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("loadEnvironment() error = %v", err)
	}
	if !env.manual {
		t.Fatal("manual = false, want true")
	}
}

func TestSelectReleasesForStableTagSkipsDrafts(t *testing.T) {
	t.Parallel()
	releases := []githubRelease{
		{TagName: "draft", Draft: true},
		{TagName: "v1.4.2"},
		{TagName: "202608111500-13a-12345678"},
	}
	env := environment{
		commit:  "abcdef1234567890",
		refType: "tag",
		refName: "v1.4.2",
	}

	selection, err := selectReleases(releases, env)
	if err != nil {
		t.Fatalf("selectReleases() error = %v", err)
	}
	if selection.target != "v1.4.2" || selection.previous != releases[2].TagName {
		t.Fatalf("selection = %#v, want stable target and preceding release", selection)
	}
}

func TestStateReportsApplied(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "update-state.json")
	if err := selfupdate.SaveState(statePath, selfupdate.State{LastResult: "applied"}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	applied, err := stateReportsApplied(statePath)
	if err != nil {
		t.Fatalf("stateReportsApplied() error = %v", err)
	}
	if !applied {
		t.Fatal("stateReportsApplied() = false, want true")
	}
}

func TestBinaryVersionIgnoresStderr(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "main.go")
	binaryPath := filepath.Join(directory, "version-helper")
	source := `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stdout, "clyde version test-release")
	fmt.Fprintln(os.Stderr, "diagnostic")
}
`
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if output, err := commandOutput(t.Context(), directory, nil, "go", "build", "-o", binaryPath, sourcePath); err != nil {
		t.Fatalf("go build error = %v, output = %q", err, output)
	}

	version, err := binaryVersion(t.Context(), directory, binaryPath)
	if err != nil {
		t.Fatalf("binaryVersion() error = %v", err)
	}
	if version != "clyde version test-release" {
		t.Fatalf("binaryVersion() = %q, want stdout only", version)
	}
}

func TestRemoveTestRootRejectsTempDirectory(t *testing.T) {
	t.Parallel()
	err := removeTestRoot(os.TempDir())
	if err == nil {
		t.Fatal("removeTestRoot() error = nil, want refusal")
	}
}

func TestRemoveTestRootRemovesOwnedDirectory(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp(testRootParent, testRootPattern)
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	if err := removeTestRoot(root); err != nil {
		t.Fatalf("removeTestRoot() error = %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("Stat() error = %v, want not exist", err)
	}
}

func TestServiceManagerInvocation(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/tmp/launchctl", "/tmp/systemctl"} {
		if !isServiceManagerInvocation(path) {
			t.Fatalf("isServiceManagerInvocation(%q) = false, want true", path)
		}
	}
	if isServiceManagerInvocation("/tmp/ci-auto-update") {
		t.Fatal("isServiceManagerInvocation(ci-auto-update) = true, want false")
	}
}
