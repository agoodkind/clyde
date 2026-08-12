package updateopts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	buildversion "goodkind.io/gklog/version"
	"goodkind.io/go-makefile/selfupdate"
)

type testReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type testRelease struct {
	HTMLURL    string             `json:"html_url"`
	TagName    string             `json:"tag_name"`
	Draft      bool               `json:"draft"`
	Prerelease bool               `json:"prerelease"`
	Assets     []testReleaseAsset `json:"assets"`
}

func TestLocalBuildCheckAndApplyNeverDownloadRelease(t *testing.T) {
	restoreBuildVersion := setTestBuildVersion(localBuildVersion, "false")
	t.Cleanup(restoreBuildVersion)

	var downloadCount atomic.Int32
	server := newUpdateServer(t, "202608011200-c7-abcdef0", false, &downloadCount)
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
		name             string
		currentVersion   string
		latestVersion    string
		latestPrerelease bool
	}{
		{
			name:             "release build accepts rolling prerelease",
			currentVersion:   releaseBuildVersion,
			latestVersion:    "202608011200-c7-abcdef0",
			latestPrerelease: true,
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
			server := newUpdateServer(t, test.latestVersion, test.latestPrerelease, &downloadCount)
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
	options.Config.APIBaseURL = server.URL
	options.StatePath = filepath.Join(stateDir, "state.json")
	options.CacheDir = filepath.Join(stateDir, "cache")
	return options
}

func newUpdateServer(
	t *testing.T,
	latestVersion string,
	prerelease bool,
	downloadCount *atomic.Int32,
) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/agoodkind/clyde/releases":
			response := []testRelease{{
				HTMLURL:    server.URL + "/release",
				TagName:    latestVersion,
				Prerelease: prerelease,
				Assets: []testReleaseAsset{{
					Name:               "clyde_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz",
					BrowserDownloadURL: server.URL + "/download/clyde.tar.gz",
				}},
			}}
			if err := json.NewEncoder(writer).Encode(response); err != nil {
				t.Errorf("encode release response: %v", err)
			}
		case "/download/clyde.tar.gz":
			downloadCount.Add(1)
			_, _ = writer.Write([]byte("unexpected download"))
		default:
			http.NotFound(writer, request)
		}
	}))
	return server
}
