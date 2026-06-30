package codex

import "testing"

func TestApplyPatchArgsRepairsNestedAddFileHeaderInsideUpdateWrapper(t *testing.T) {
	input := "*** Begin Patch\n" +
		"*** Update File: /tmp/render_scope_test.go\n" +
		"*** Add File: /tmp/render_scope_test.go\n" +
		"+package tools\n" +
		"*** End Patch\n"

	got, ok := ApplyPatchArgs(input)
	if !ok {
		t.Fatal("ApplyPatchArgs returned !ok")
	}
	want := "*** Begin Patch\n" +
		"*** Add File: /tmp/render_scope_test.go\n" +
		"+package tools\n" +
		"*** End Patch\n"
	if got != want {
		t.Fatalf("repaired patch:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyPatchArgsKeepsNormalUpdatePatch(t *testing.T) {
	input := "*** Begin Patch\n" +
		"*** Update File: existing.go\n" +
		"@@\n" +
		"-old\n" +
		"+new\n" +
		"*** End Patch\n"

	got, ok := ApplyPatchArgs(input)
	if !ok {
		t.Fatal("ApplyPatchArgs returned !ok")
	}
	if got != input {
		t.Fatalf("normal update patch changed:\n%s", got)
	}
}
