package searchacceptance_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/searchacceptance"
)

func TestComparisonCommandRejectsRegression(t *testing.T) {
	baselinePath, batteryPath, report := savedReport(t)
	root := filepath.Dir(baselinePath)
	binaryPath := filepath.Join(root, "embedded-search-acceptance")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binaryPath, "../../cmd/embedded-search-acceptance")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public comparison command: %v: %s", err, output)
	}
	candidatePath := filepath.Join(root, "candidate.json")
	writeJSON(t, candidatePath, report)
	control, err := comparisonCommand(t, binaryPath, batteryPath, baselinePath, candidatePath)
	if err != nil || !control.WorkloadCompatible || !control.PerformancePassed || control.AcceptanceComplete {
		t.Fatalf("public control comparison failed: %+v, %v", control, err)
	}
	report.Traversals[0].Pages[0].ElapsedMS++
	writeJSON(t, candidatePath, report)
	result, err := comparisonCommand(t, binaryPath, batteryPath, baselinePath, candidatePath)
	if err == nil || result.PerformancePassed || !result.WorkloadCompatible || len(result.Measurements) == 0 {
		t.Fatalf("public command accepted latency regression or omitted evidence: %+v, %v", result, err)
	}
}

func TestSnapshotCommandRejectsChangedSource(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "embedded-search-acceptance")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/embedded-search-acceptance")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build snapshot verification command: %v: %s", err, output)
	}
	content := []byte("frozen transcript\n")
	source := filepath.Join(root, "source.jsonl")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(searchacceptance.Digest(content) + "  ./source.jsonl\n")
	manifestPath := filepath.Join(root, "MANIFEST.sha256")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"verify-snapshot", "--root", root, "--manifest", manifestPath, "--sha256", searchacceptance.Digest(manifest)}
	command := exec.CommandContext(t.Context(), binary, arguments...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("verify frozen source command: %v", err)
	}
	var result searchacceptance.SnapshotVerification
	if err := json.Unmarshal(output, &result); err != nil || result.Files != 1 || result.Bytes != int64(len(content)) {
		t.Fatalf("invalid source verification result: %+v, %v", result, err)
	}
	if err := os.WriteFile(source, []byte("changed transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(t.Context(), binary, arguments...).Run(); err == nil {
		t.Fatal("public snapshot command accepted changed source")
	}
}

func TestComparisonCommandValidatesEffectivePageLimit(t *testing.T) {
	baselinePath, batteryPath, report := savedReport(t)
	root := filepath.Dir(baselinePath)
	binaryPath := filepath.Join(root, "embedded-search-acceptance")
	build := exec.CommandContext(t.Context(), "go", "build", "-p", "1", "-o", binaryPath, "../../cmd/embedded-search-acceptance")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public comparison command: %v: %s", err, output)
	}
	const requestedLimit = 100
	const effectiveLimit = 50
	const total = 251
	identities := make([]string, total)
	for index := range identities {
		identities[index] = fmt.Sprintf("occurrence-%03d", index)
	}
	battery := searchacceptance.Battery{Concurrency: 1, TimeoutMS: 30000, Entries: []searchacceptance.Query{
		{ID: "query", Query: "storage", PageSize: requestedLimit, ExpectedOccurrenceIDs: identities, ExpectedTotal: total},
	}}
	batteryBytes := writeJSON(t, batteryPath, battery)
	report.BatteryDigest = searchacceptance.Digest(batteryBytes)
	for traversalIndex := range report.Traversals {
		report.Traversals[traversalIndex].Total = total
		report.Traversals[traversalIndex].Pages = nil
		for start := 0; start < total; start += effectiveLimit {
			end := min(start+effectiveLimit, total)
			limit := effectiveLimit
			report.Traversals[traversalIndex].Pages = append(report.Traversals[traversalIndex].Pages, searchacceptance.Page{
				OccurrenceIDs: identities[start:end], ElapsedMS: 1, HasMore: end < total, Limit: &limit,
			})
		}
	}
	writeJSON(t, baselinePath, report)
	candidatePath := filepath.Join(root, "candidate.json")
	writeJSON(t, candidatePath, report)
	result, err := comparisonCommand(t, binaryPath, batteryPath, baselinePath, candidatePath)
	if err != nil || !result.WorkloadCompatible || !result.PerformancePassed || result.AcceptanceComplete {
		t.Fatalf("public command rejected complete clamped pages: %+v, %v", result, err)
	}
	_, decodedBattery, err := searchacceptance.ReadReport(candidatePath, batteryPath)
	if err != nil || decodedBattery.Entries[0].PageSize != requestedLimit {
		t.Fatalf("recorded effective limit replaced the original request: %+v, %v", decodedBattery, err)
	}
	for _, invalidLimit := range []int{0, 25} {
		report.Traversals[0].Pages[1].Limit = &invalidLimit
		writeJSON(t, candidatePath, report)
		if _, err := comparisonCommand(t, binaryPath, batteryPath, baselinePath, candidatePath); err == nil {
			t.Fatalf("public command accepted invalid continuation limit %d", invalidLimit)
		}
	}
	limit := effectiveLimit
	report.Traversals[0].Pages[1].Limit = &limit
	report.Traversals[0].Pages[0].Limit = nil
	writeJSON(t, candidatePath, report)
	if _, err := comparisonCommand(t, binaryPath, batteryPath, baselinePath, candidatePath); err == nil {
		t.Fatal("public command accepted an omitted first limit with explicit continuation limits")
	}
	report.Traversals[0].Pages[0].Limit = &limit
	report.Traversals[0].Pages[1].Limit = nil
	writeJSON(t, candidatePath, report)
	if _, err := comparisonCommand(t, binaryPath, batteryPath, baselinePath, candidatePath); err == nil {
		t.Fatal("public command accepted an omitted continuation limit")
	}
	report.Traversals[0].Pages[1].Limit = &limit
	content := writeJSON(t, candidatePath, report)
	content = bytes.Replace(content, []byte(`"limit":50`), []byte(`"limit":null`), 1)
	if err := os.WriteFile(candidatePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := comparisonCommand(t, binaryPath, batteryPath, baselinePath, candidatePath); err == nil {
		t.Fatal("public command accepted an explicit null limit")
	}
}

func comparisonCommand(t *testing.T, binary, battery, baseline, candidate string) (searchacceptance.Comparison, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binary, "--battery", battery, "--baseline", baseline, "--candidate", candidate)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var result searchacceptance.Comparison
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("decode public comparison result: %v: %s: %s", decodeErr, stdout.String(), stderr.String())
	}
	return result, err
}
