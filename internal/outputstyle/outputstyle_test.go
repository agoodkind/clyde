package outputstyle

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGetCustomStylePathEncodesIdentifier(t *testing.T) {
	got := GetCustomStylePath("/tmp/clyde", "Merry Swan / follow-up")
	if !strings.HasPrefix(got, filepath.Join("/tmp/clyde", "..", "output-styles", "clyde")+string(filepath.Separator)) {
		t.Fatalf("GetCustomStylePath prefix = %q", got)
	}
	if strings.Contains(filepath.Base(got), "/") {
		t.Fatalf("encoded output style file contains slash: %q", got)
	}
	if !strings.HasSuffix(got, ".md") {
		t.Fatalf("GetCustomStylePath suffix = %q, want .md", got)
	}
}
