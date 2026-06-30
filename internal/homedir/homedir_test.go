package homedir

import (
	"path/filepath"
	"testing"
)

func TestExpandResolvesLeadingTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := map[string]string{
		"":            "",
		"~":           home,
		"~/":          home,
		"~/.codex":    filepath.Join(home, ".codex"),
		"~/a/b/c.txt": filepath.Join(home, "a", "b", "c.txt"),
		"/etc/hosts":  "/etc/hosts",
		"relative":    "relative",
		"~user/x":     "~user/x",
	}
	for input, want := range cases {
		if got := Expand(input); got != want {
			t.Errorf("Expand(%q)=%q want %q", input, got, want)
		}
	}
}

func TestContractRendersHomeRelativeDisplay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := map[string]string{
		home:                          "~",
		filepath.Join(home, ".codex"): "~/.codex",
		filepath.Join(home, "a", "b"): "~/a/b",
		"/etc/hosts":                  "/etc/hosts",
	}
	for input, want := range cases {
		if got := Contract(input); got != want {
			t.Errorf("Contract(%q)=%q want %q", input, got, want)
		}
	}
}

func TestExpandContractRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	absolute := filepath.Join(home, "state", "clyde", "capture.db")
	display := Contract(absolute)
	if display != "~/state/clyde/capture.db" {
		t.Fatalf("Contract(%q)=%q", absolute, display)
	}
	if back := Expand(display); back != absolute {
		t.Fatalf("Expand(%q)=%q want %q", display, back, absolute)
	}
}
