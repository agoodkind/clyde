package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/searchacceptance"
)

func TestInspectBatteryPublicCommand(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "embedded-search-acceptance")
	build := exec.CommandContext(t.Context(), "go", "build", "-p", "1", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build battery inspector: %v\n%s", err, output)
	}
	original, err := os.ReadFile("testdata/frozen-battery.json")
	if err != nil {
		t.Fatal(err)
	}
	var frozen searchacceptance.FrozenBattery
	if err := json.Unmarshal(original, &frozen); err != nil {
		t.Fatal(err)
	}
	t.Run("preserve complete original workload", func(t *testing.T) {
		output, stderr, err := invokeBatteryInspector(t, binary, original, searchacceptance.Digest(original))
		if err != nil {
			t.Fatalf("inspect battery: %v\n%s", err, stderr)
		}
		var inspected searchacceptance.BatteryInspection
		if err := json.Unmarshal(output, &inspected); err != nil {
			t.Fatal(err)
		}
		if inspected.OriginalDigest != searchacceptance.Digest(original) || !reflect.DeepEqual(inspected.Battery, frozen) {
			t.Fatal("inspection changed original controls or provenance")
		}
		if inspected.Battery.Entries[0].Filter.ConversationIDs != nil || inspected.Battery.Entries[48].Filter.ConversationIDs == nil || len(inspected.Battery.Entries[48].Filter.ConversationIDs) != 0 {
			t.Fatal("null and explicit empty membership differ from original")
		}
		for _, forbidden := range []string{"expected_occurrence_ids", "acceptance_complete", "\"complete\""} {
			if bytes.Contains(output, []byte(forbidden)) {
				t.Fatalf("inspection asserts %s without an oracle", forbidden)
			}
		}
	})
	cases := []struct {
		name   string
		change func(*searchacceptance.FrozenBattery)
	}{
		{"duplicate query", func(b *searchacceptance.FrozenBattery) { b.Entries[1].ID = b.Entries[0].ID }},
		{"missing query", func(b *searchacceptance.FrozenBattery) { b.Entries = b.Entries[:50] }},
		{"unknown query ID", func(b *searchacceptance.FrozenBattery) { b.Entries[0].ID = "q999" }},
		{"duplicate scenario", func(b *searchacceptance.FrozenBattery) { b.Scenarios[1].ID = b.Scenarios[0].ID }},
		{"missing scenario", func(b *searchacceptance.FrozenBattery) { b.Scenarios = b.Scenarios[:2] }},
		{"unknown scenario query", func(b *searchacceptance.FrozenBattery) { b.Scenarios[0].QueryIDs = []string{"q999"} }},
		{"malformed date", func(b *searchacceptance.FrozenBattery) { value := "2026-99-01"; b.Entries[0].Filter.After = &value }},
		{"negative group limit", func(b *searchacceptance.FrozenBattery) {
			value := -1
			b.Entries[0].Filter.PerConversationLimit = &value
		}},
		{"negative score", func(b *searchacceptance.FrozenBattery) { value := -0.1; b.Entries[0].Filter.MinScore = &value }},
		{"zero page limit", func(b *searchacceptance.FrozenBattery) { b.Entries[0].PageSize = 0 }},
		{"zero timeout", func(b *searchacceptance.FrozenBattery) { b.PageTimeoutSeconds = 0 }},
		{"reversed timeout", func(b *searchacceptance.FrozenBattery) { b.TraversalTimeoutSeconds = 1 }},
		{"zero concurrency", func(b *searchacceptance.FrozenBattery) { b.LatencyConcurrency = 0 }},
		{"invalid provenance", func(b *searchacceptance.FrozenBattery) { b.CorpusSnapshot.ManifestSHA256 = "invalid" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var candidate searchacceptance.FrozenBattery
			if err := json.Unmarshal(original, &candidate); err != nil {
				t.Fatal(err)
			}
			test.change(&candidate)
			content, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			assertBatteryRejected(t, binary, content, searchacceptance.Digest(content))
		})
	}
	t.Run("bad digest", func(t *testing.T) { assertBatteryRejected(t, binary, original, strings.Repeat("0", 64)) })
	t.Run("unknown field", func(t *testing.T) {
		content := bytes.Replace(original, []byte("\"schema_version\": 1,"), []byte("\"unknown\": true,\"schema_version\": 1,"), 1)
		assertBatteryRejected(t, binary, content, searchacceptance.Digest(content))
	})
	t.Run("trailing JSON", func(t *testing.T) {
		content := append(bytes.Clone(original), []byte("\n{}")...)
		assertBatteryRejected(t, binary, content, searchacceptance.Digest(content))
	})
}

func invokeBatteryInspector(t *testing.T, binary string, content []byte, digest string) ([]byte, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "original.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "inspect-battery", "--battery", path, "--sha256", digest)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	return output, stderr.String(), err
}

func assertBatteryRejected(t *testing.T, binary string, content []byte, digest string) {
	t.Helper()
	output, stderr, err := invokeBatteryInspector(t, binary, content, digest)
	if err == nil || len(output) != 0 || stderr == "" {
		t.Fatalf("invalid battery accepted or rejection omitted: err=%v stdout=%s stderr=%s", err, output, stderr)
	}
}
