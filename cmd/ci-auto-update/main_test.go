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
