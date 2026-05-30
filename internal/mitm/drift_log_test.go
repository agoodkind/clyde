package mitm

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendDriftOutcomeWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "mitm-drift.jsonl")

	v2 := DiffReportV2{Upstream: "claude-code"}
	first := DriftOutcome{
		Upstream:       "claude-code",
		SchemaVersion:  "v2",
		ReferencePath:  "/refs/claude.toml",
		TranscriptPath: "/tx/a.jsonl",
		StartedAt:      time.Date(2026, 4, 28, 7, 30, 0, 0, time.UTC),
		V2:             &v2,
	}
	if err := AppendDriftOutcome(path, first); err != nil {
		t.Fatalf("first append: %v", err)
	}

	v2drift := DiffReportV2{Upstream: "claude-code", MissingFlavors: []string{"flav-x"}}
	second := DriftOutcome{
		Upstream:       "claude-code",
		SchemaVersion:  "v2",
		ReferencePath:  "/refs/claude.toml",
		TranscriptPath: "/tx/b.jsonl",
		StartedAt:      time.Date(2026, 4, 29, 7, 30, 0, 0, time.UTC),
		V2:             &v2drift,
	}
	if err := AppendDriftOutcome(path, second); err != nil {
		t.Fatalf("second append: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var lines []DriftOutcome
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var o DriftOutcome
		if err := json.Unmarshal(scanner.Bytes(), &o); err != nil {
			t.Fatalf("decode: %v line=%s", err, scanner.Text())
		}
		lines = append(lines, o)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0].Diverged {
		t.Errorf("first outcome should not be diverged")
	}
	if !lines[1].Diverged {
		t.Errorf("second outcome should be diverged")
	}
	if lines[0].Timestamp.IsZero() {
		t.Errorf("first timestamp should be auto-populated")
	}
	if !strings.Contains(lines[1].Summary, "DRIFT") {
		t.Errorf("second summary missing DRIFT marker: %q", lines[1].Summary)
	}
}

func TestAppendDriftOutcomeRejectsEmptyPath(t *testing.T) {
	if err := AppendDriftOutcome("", DriftOutcome{}); err == nil {
		t.Fatal("expected error on empty path")
	}
}

// TestAppendDriftOutcomeExpandsHome guards the bug where a drift_log_dir of
// "~/..." was used verbatim, so the mkdir tried to create a literal "~" dir and
// the outcome log was never written. The path must be home-expanded.
func TestAppendDriftOutcomeExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := filepath.Join("~", "mitm-drift", "claude-code.jsonl")
	outcome := DriftOutcome{
		Upstream:      "claude-code",
		SchemaVersion: "v2",
		V2:            &DiffReportV2{Upstream: "claude-code"},
	}
	if err := AppendDriftOutcome(path, outcome); err != nil {
		t.Fatalf("append with ~ path: %v", err)
	}

	want := filepath.Join(home, "mitm-drift", "claude-code.jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected outcome at expanded path %q: %v", want, err)
	}
	if _, err := os.Stat(filepath.Join("~", "mitm-drift")); err == nil {
		t.Fatal("a literal ~ directory was created; path was not home-expanded")
	}
}
