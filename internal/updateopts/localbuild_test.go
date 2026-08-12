package updateopts

import (
	"testing"

	buildversion "goodkind.io/gklog/version"
)

const (
	localBuildVersion   = "202607281237-b5-e82aad0-9-g6586a88"
	releaseBuildVersion = "202607301700-b6-700f20b"
)

func TestLocalDeployIsNeverAutoReplaced(t *testing.T) {
	if !isLocalBuild(localBuildVersion, false) {
		t.Fatal("clean local build was treated as updateable")
	}
	if isLocalBuild(releaseBuildVersion, false) {
		t.Fatal("release build was treated as local")
	}
}

func TestIsLocalBuild(t *testing.T) {
	tests := []struct {
		name    string
		version string
		dirty   bool
		want    bool
	}{
		{name: "release tag", version: releaseBuildVersion, want: false},
		{name: "release tag built dirty", version: releaseBuildVersion, dirty: true, want: true},
		{name: "clean build ahead of a tag", version: localBuildVersion, want: true},
		{name: "one commit ahead", version: "202607301700-b6-700f20b-1-gabc1234", want: true},
		{name: "unstamped dev", version: "dev", want: true},
		{name: "unstamped unknown", version: "unknown", want: true},
		{name: "empty", version: "", want: true},
		{name: "semver release", version: "v1.4.2", want: false},
		{name: "semver ahead of tag", version: "v1.4.2-3-gdeadbee", want: true},
		{name: "trailing g without hex", version: "202607301700-b6-gzzzz", want: false},
		{name: "trailing hex without commit count", version: "202607301700-b6-700f20b-gabc1234", want: false},
		{name: "count field is not a number", version: "202607301700-b6-x-gabc1234", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := isLocalBuild(test.version, test.dirty)
			if got != test.want {
				t.Fatalf("isLocalBuild(%q, %v) = %v, want %v", test.version, test.dirty, got, test.want)
			}
		})
	}
}

func TestOptionsCarriesLocalBuildIntoCurrentDirty(t *testing.T) {
	originalVersion := buildversion.Version
	originalDirty := buildversion.Dirty
	t.Cleanup(func() {
		buildversion.Version = originalVersion
		buildversion.Dirty = originalDirty
	})
	buildversion.Version = localBuildVersion
	buildversion.Dirty = "false"

	options := Options(Overrides{})

	if !options.Config.CurrentDirty {
		t.Fatal("CurrentDirty = false, want true for clean local build")
	}
}
