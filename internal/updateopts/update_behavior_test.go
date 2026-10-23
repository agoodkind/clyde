package updateopts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
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

func TestNetworkOptionsSendsEnvironmentTokenToReleaseAPI(t *testing.T) {
	tests := []struct {
		name        string
		ghToken     string
		githubToken string
		wantToken   string
	}{
		{
			name:      "GH_TOKEN with surrounding whitespace",
			ghToken:   "  gh-token-value \n",
			wantToken: "gh-token-value",
		},
		{
			name:        "GITHUB_TOKEN when GH_TOKEN is empty",
			githubToken: "github-token-value",
			wantToken:   "github-token-value",
		},
		{
			name:        "GH_TOKEN before GITHUB_TOKEN",
			ghToken:     "gh-token-value",
			githubToken: "github-token-value",
			wantToken:   "gh-token-value",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restoreBuildVersion := setTestBuildVersion(releaseBuildVersion, "false")
			t.Cleanup(restoreBuildVersion)
			t.Setenv("GH_TOKEN", test.ghToken)
			t.Setenv("GITHUB_TOKEN", test.githubToken)

			var downloadCount atomic.Int32
			server := newTokenUpdateServer(t, "202608011200-c7-abcdef0", test.wantToken, &downloadCount)
			t.Cleanup(server.Close)

			result, err := selfupdate.Check(context.Background(), networkTestOptions(t, server))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if !result.UpdateAvailable {
				t.Fatal("Check() UpdateAvailable = false, want true")
			}
		})
	}
}

func TestNetworkOptionsRejectedTokenErrorOmitsToken(t *testing.T) {
	restoreBuildVersion := setTestBuildVersion(releaseBuildVersion, "false")
	t.Cleanup(restoreBuildVersion)
	rejectedToken := "rejected-token-value"
	t.Setenv("GH_TOKEN", rejectedToken)
	t.Setenv("GITHUB_TOKEN", "")

	var downloadCount atomic.Int32
	server := newTokenUpdateServer(t, "202608011200-c7-abcdef0", "accepted-token-value", &downloadCount)
	t.Cleanup(server.Close)

	_, err := selfupdate.Check(context.Background(), networkTestOptions(t, server))
	if err == nil {
		t.Fatal("Check() error = nil, want HTTP 403 error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("Check() error = %q, want HTTP 403", err)
	}
	if strings.Contains(err.Error(), rejectedToken) {
		t.Fatalf("Check() error = %q, want error without the token", err)
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
	return newReleaseServer(t, latestVersion, prerelease, "", downloadCount)
}

func newTokenUpdateServer(
	t *testing.T,
	latestVersion string,
	requiredToken string,
	downloadCount *atomic.Int32,
) *httptest.Server {
	t.Helper()
	return newReleaseServer(t, latestVersion, true, requiredToken, downloadCount)
}

func newReleaseServer(
	t *testing.T,
	latestVersion string,
	prerelease bool,
	requiredToken string,
	downloadCount *atomic.Int32,
) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requiredToken != "" && request.Header.Get("Authorization") != "Bearer "+requiredToken {
			http.Error(writer, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
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
