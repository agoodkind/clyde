package updateopts

import (
	"log/slog"
	"net/http"
	"reflect"
	"testing"

	"goodkind.io/gklog/version"
)

func TestOptionsUseClydeReleaseIdentityAndLibraryDefaultPaths(t *testing.T) {
	options := Options(Overrides{})

	if options.Config.Repo != "agoodkind/clyde" {
		t.Fatalf("Repo = %q, want agoodkind/clyde", options.Config.Repo)
	}
	if options.Config.Binary != "clyde" {
		t.Fatalf("Binary = %q, want clyde", options.Config.Binary)
	}
	if options.Config.CurrentVersion != version.Version {
		t.Fatalf("CurrentVersion = %q, want %q", options.Config.CurrentVersion, version.Version)
	}
	if options.Config.CurrentCommit != version.Commit {
		t.Fatalf("CurrentCommit = %q, want %q", options.Config.CurrentCommit, version.Commit)
	}
	if options.Config.CurrentBuildHash != version.BuildHash() {
		t.Fatalf("CurrentBuildHash = %q, want %q", options.Config.CurrentBuildHash, version.BuildHash())
	}
	wantCurrentDirty := isLocalBuild(version.Version, version.Dirty == "true")
	if options.Config.CurrentDirty != wantCurrentDirty {
		t.Fatalf("CurrentDirty = %v, want %v", options.Config.CurrentDirty, wantCurrentDirty)
	}
	if options.Config.AllowPrerelease != nil {
		t.Fatal("AllowPrerelease is non-nil, want library rolling default")
	}
	if !reflect.DeepEqual(options.Config.ValidateArgs, []string{"--version"}) {
		t.Fatalf("ValidateArgs = %v, want [--version]", options.Config.ValidateArgs)
	}
	if options.Config.ValidateMatch != "clyde version" {
		t.Fatalf("ValidateMatch = %q, want clyde version", options.Config.ValidateMatch)
	}
	if options.StatePath != "" {
		t.Fatalf("StatePath = %q, want library default", options.StatePath)
	}
	if options.CacheDir != "" {
		t.Fatalf("CacheDir = %q, want library default", options.CacheDir)
	}
}

func TestOptionsPreserveOperationOverrides(t *testing.T) {
	client := &http.Client{}
	logger := slog.Default()

	options := Options(Overrides{
		Client:      client,
		InstallPath: "/tmp/clyde",
		DryRun:      true,
		Log:         logger,
	})

	if options.Client != client {
		t.Fatal("Client override was not preserved")
	}
	if options.InstallPath != "/tmp/clyde" {
		t.Fatalf("InstallPath = %q, want /tmp/clyde", options.InstallPath)
	}
	if !options.DryRun {
		t.Fatal("DryRun = false, want true")
	}
	if options.Log != logger {
		t.Fatal("Log override was not preserved")
	}
}
