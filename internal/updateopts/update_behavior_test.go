package updateopts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	buildversion "goodkind.io/gklog/version"
	"goodkind.io/go-makefile/selfupdate"
)

func TestLocalBuildCheckAndApplyNeverDownloadRelease(t *testing.T) {
	restoreBuildVersion := setTestBuildVersion(localBuildVersion, "false")
	t.Cleanup(restoreBuildVersion)

	var downloadCount atomic.Int32
	server := newUpdateServer(t, "202608011200-c7-abcdef0", &downloadCount)
	t.Cleanup(server.Close)
	options := updateTestOptions(t, server)

	checkResult, err := selfupdate.Check(context.Background(), options)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if checkResult.UpdateAvailable || !checkResult.DevBuild {
		t.Fatalf(
			"Check() UpdateAvailable=%v DevBuild=%v, want false/true",
			checkResult.UpdateAvailable,
			checkResult.DevBuild,
		)
	}

	applyResult, err := selfupdate.Apply(context.Background(), options)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if applyResult.UpdateAvailable || applyResult.Applied {
		t.Fatalf(
			"Apply() UpdateAvailable=%v Applied=%v, want false/false",
			applyResult.UpdateAvailable,
			applyResult.Applied,
		)
	}
	if got := downloadCount.Load(); got != 0 {
		t.Fatalf("release download count = %d, want 0", got)
	}
}

func TestCIReleaseAndPrereleaseBuildsRemainUpdateable(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion string
		latestVersion  string
	}{
		{
			name:           "release build accepts rolling prerelease",
			currentVersion: releaseBuildVersion,
			latestVersion:  "202608011200-c7-abcdef0",
		},
		{
			name:           "prerelease build accepts stable release",
			currentVersion: "v1.4.2-rc.1",
			latestVersion:  "v1.4.2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restoreBuildVersion := setTestBuildVersion(test.currentVersion, "false")
			t.Cleanup(restoreBuildVersion)

			var downloadCount atomic.Int32
			server := newUpdateServer(t, test.latestVersion, &downloadCount)
			t.Cleanup(server.Close)

			result, err := selfupdate.Check(context.Background(), updateTestOptions(t, server))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if !result.UpdateAvailable || result.DevBuild {
				t.Fatalf(
					"Check() UpdateAvailable=%v DevBuild=%v, want true/false",
					result.UpdateAvailable,
					result.DevBuild,
				)
			}
		})
	}
}

func TestNetworkOptionsDoesNotSendCredentials(t *testing.T) {
	restoreBuildVersion := setTestBuildVersion(releaseBuildVersion, "false")
	t.Cleanup(restoreBuildVersion)
	t.Setenv("GH_TOKEN", "example-token")
	t.Setenv("GITHUB_TOKEN", "another-token")

	var downloadCount atomic.Int32
	server := newUpdateServer(t, "202608011200-c7-abcdef0", &downloadCount)
	t.Cleanup(server.Close)

	result, err := selfupdate.Check(context.Background(), networkTestOptions(t, server))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.UpdateAvailable {
		t.Fatal("Check() did not report the newer release")
	}
}

func setTestBuildVersion(currentVersion string, dirty string) func() {
	originalVersion := buildversion.Version
	originalDirty := buildversion.Dirty
	buildversion.Version = currentVersion
	buildversion.Dirty = dirty
	return func() {
		buildversion.Version = originalVersion
		buildversion.Dirty = originalDirty
	}
}

func updateTestOptions(t *testing.T, server *httptest.Server) selfupdate.Options {
	t.Helper()
	stateDir := t.TempDir()
	options := Options(Overrides{
		Client:      server.Client(),
		InstallPath: filepath.Join(stateDir, "clyde"),
	})
	return withTestServerPaths(options, server, stateDir)
}

func networkTestOptions(t *testing.T, server *httptest.Server) selfupdate.Options {
	t.Helper()
	stateDir := t.TempDir()
	options := NetworkOptions(context.Background(), Overrides{
		Client:      server.Client(),
		InstallPath: filepath.Join(stateDir, "clyde"),
	})
	return withTestServerPaths(options, server, stateDir)
}

func withTestServerPaths(options selfupdate.Options, server *httptest.Server, stateDir string) selfupdate.Options {
	options.Config.WebBaseURL = server.URL
	options.StatePath = filepath.Join(stateDir, "state.json")
	options.CacheDir = filepath.Join(stateDir, "cache")
	return options
}

func newUpdateServer(
	t *testing.T,
	latestVersion string,
	downloadCount *atomic.Int32,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			t.Error("The update request included an authorization header")
		}
		switch request.URL.Path {
		case "/agoodkind/clyde/releases.atom":
			_, _ = writer.Write([]byte("<feed><entry><id>tag:github.com,2008:Repository/1/" + latestVersion + "</id></entry></feed>"))
		case "/agoodkind/clyde/releases/download/" + latestVersion + "/checksums.txt":
			asset := "clyde_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
			_, _ = writer.Write([]byte("deadbeef  " + asset + "\n"))
		default:
			downloadCount.Add(1)
			http.NotFound(writer, request)
		}
	}))
}
