// Package updateopts adapts Clyde build identity to selfupdate options.
package updateopts

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

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

// NetworkOptions builds options for a network operation and reuses credentials
// already available in the environment or GitHub CLI.
func NetworkOptions(ctx context.Context, overrides Overrides) selfupdate.Options {
	options := Options(overrides)
	logger := overrides.Log
	if logger == nil {
		logger = slog.Default()
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			logger.DebugContext(ctx, "update.credentials.resolved", "source", name)
			options.Config.AuthToken = token
			return options
		}
	}
	helperContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(helperContext, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err == nil {
		options.Config.AuthToken = strings.TrimSpace(string(output))
		logger.DebugContext(ctx, "update.credentials.resolved", "source", "gh")
	} else {
		logger.DebugContext(ctx, "update.credentials.unavailable", "source", "gh")
	}
	return options
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
