package claude

import "testing"

func TestCustomTitleNameGetNameTrimsWhitespace(t *testing.T) {
	name := CustomTitleName{Title: "  Merry Swan  "}
	if got := name.GetName(); got != "Merry Swan" {
		t.Fatalf("GetName() = %q, want %q", got, "Merry Swan")
	}
}

func TestCustomTitleNameRenamePreservesExactDisplayName(t *testing.T) {
	name := CustomTitleName{Title: "Merry Swan"}
	got := name.Rename("", map[string]bool{})
	if got != "Merry Swan" {
		t.Fatalf("Rename() = %q, want %q", got, "Merry Swan")
	}
}

func TestCustomTitleNameRenameDedupesExactDisplayName(t *testing.T) {
	name := CustomTitleName{Title: "Merry Swan"}
	got := name.Rename("", map[string]bool{"Merry Swan": true})
	if got != "Merry Swan (2)" {
		t.Fatalf("Rename() = %q, want %q", got, "Merry Swan (2)")
	}
}

func TestCustomTitleNameRenameRejectsEmptyTitles(t *testing.T) {
	name := CustomTitleName{Title: "   "}
	if got := name.Rename("", map[string]bool{}); got != "" {
		t.Fatalf("Rename() = %q, want empty string", got)
	}
}

func TestCustomTitleNameRenameRejectsInvalidDisplayNames(t *testing.T) {
	name := CustomTitleName{Title: "bad\nname"}
	if got := name.Rename("", map[string]bool{}); got != "" {
		t.Fatalf("Rename() = %q, want empty string", got)
	}
}
