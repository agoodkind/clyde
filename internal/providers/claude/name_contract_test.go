package claude

import "testing"

func TestCustomTitleNameGetNameTrimsWhitespace(t *testing.T) {
	name := CustomTitleName{Title: "  Merry Swan  "}
	if got := name.GetName(); got != "Merry Swan" {
		t.Fatalf("GetName() = %q, want %q", got, "Merry Swan")
	}
}

func TestCustomTitleNameRenameSanitizesAndDedupes(t *testing.T) {
	name := CustomTitleName{Title: "Merry Swan"}
	got := name.Rename("", map[string]bool{"merry-swan": true})
	if got != "merry-swan-2" {
		t.Fatalf("Rename() = %q, want %q", got, "merry-swan-2")
	}
}

func TestCustomTitleNameRenameRejectsEmptyTitles(t *testing.T) {
	name := CustomTitleName{Title: "🙂🎉"}
	if got := name.Rename("", map[string]bool{}); got != "" {
		t.Fatalf("Rename() = %q, want empty string", got)
	}
}
