// Package updateopts adapts Clyde build identity to selfupdate options.
package updateopts

import (
	"log/slog"
	"net/http"

	"goodkind.io/gklog/version"
	"goodkind.io/go-makefile/selfupdate"
)

// Overrides carries operation-specific update settings.
type Overrides struct {
	Client      *http.Client
	InstallPath string
	DryRun      bool
	Log         *slog.Logger
}

// Options builds selfupdate options for Clyde while leaving state and cache
// paths to the library defaults.
func Options(overrides Overrides) selfupdate.Options {
	return selfupdate.Options{
		Config: selfupdate.Config{
			Repo:             "agoodkind/clyde",
			Binary:           "clyde",
			CurrentVersion:   version.Version,
			CurrentCommit:    version.Commit,
			CurrentBuildHash: version.BuildHash(),
			CurrentDirty:     isLocalBuild(version.Version, version.Dirty == "true"),
			AllowPrerelease:  nil,
			ValidateArgs:     []string{"--version"},
			ValidateMatch:    "clyde version",
		},
		Client:      overrides.Client,
		InstallPath: overrides.InstallPath,
		CacheDir:    "",
		StatePath:   "",
		DryRun:      overrides.DryRun,
		Log:         overrides.Log,
	}
}
