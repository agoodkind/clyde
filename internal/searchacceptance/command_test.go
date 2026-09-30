package searchacceptance_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/searchacceptance"
)

func TestCollectionCommandRejectsIncompleteExecution(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(root, "source")
	if err := os.Mkdir(corpus, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "embedded-search-acceptance")
	build := exec.CommandContext(t.Context(), "go", "build", "-p", "1", "-o", binary, "../../cmd/embedded-search-acceptance")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public collection command: %v: %s", err, output)
	}
	command := exec.CommandContext(t.Context(), binary, "collect", "--mode", "baseline")
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "explicit clean absolute") {
		t.Fatalf("public collection accepted missing inputs: %v: %s", err, output)
	}
	report, battery, _ := savedReport(t)
	artifact := func(path string) searchacceptance.ExecutionArtifact {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return searchacceptance.ExecutionArtifact{Path: path, SHA256: searchacceptance.Digest(data)}
	}
	oracle := filepath.Join(root, "oracle.json")
	if err := os.WriteFile(oracle, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, "events.jsonl")
	actual := exec.CommandContext(t.Context(), "go", "test", "-p", "1", "-json", "-run", "^TestVerifySnapshotRejectsChangedSource$", "-count=1", "-timeout=60s", ".")
	output, err := actual.Output()
	if err != nil {
		t.Fatalf("collect actual source verification events: %v", err)
	}
	if err := os.WriteFile(events, output, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := searchacceptance.ExecutionPlan{
		SchemaVersion: 1, Mode: "baseline", Scope: "diagnostic", RuntimeRoot: root, CorpusSnapshot: corpus,
		Binary: artifact(binary), Battery: artifact(battery), Oracle: artifact(oracle), Report: artifact(report),
		Events: artifact(events), ExecutedPlan: artifact(oracle), Lifecycle: artifact(oracle),
		RequiredTests: []string{"TestRequiredFullSearchNeverExecuted"},
	}
	planPath := filepath.Join(root, "envelope.json")
	writeExecutionPlan(t, planPath, plan)
	arguments := []string{
		"collect", "--mode", "baseline", "--plan", planPath, "--sha256", artifact(planPath).SHA256,
		"--corpus", corpus, "--battery", battery, "--oracle", oracle, "--binary", binary, "--report", filepath.Join(root, "collection.json"),
	}
	unsafeArguments := append([]string(nil), arguments...)
	plan.RuntimeRoot = corpus
	writeExecutionPlan(t, planPath, plan)
	unsafeArguments[6] = artifact(planPath).SHA256
	unsafeArguments[len(unsafeArguments)-1] = filepath.Join(corpus, "collection.json")
	command = exec.CommandContext(t.Context(), binary, unsafeArguments...)
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "outside immutable corpus") {
		t.Fatalf("public collection accepted writing into frozen sources: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(corpus, "collection.json")); !os.IsNotExist(err) {
		t.Fatalf("public collection wrote a report into frozen sources: %v", err)
	}
	plan.RuntimeRoot = root
	writeExecutionPlan(t, planPath, plan)
	arguments[6] = artifact(planPath).SHA256
	command = exec.CommandContext(t.Context(), binary, arguments...)
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "TestRequiredFullSearchNeverExecuted is missing") {
		t.Fatalf("public collection accepted zero matching required tests: %v: %s", err, output)
	}
	plan.RequiredTests = []string{"TestVerifySnapshotRejectsChangedSource"}
	writeExecutionPlan(t, planPath, plan)
	arguments[6] = artifact(planPath).SHA256
	command = exec.CommandContext(t.Context(), binary, arguments...)
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "different plan or unsuccessful command") {
		t.Fatalf("public collection accepted missing lifecycle proof: %v: %s", err, output)
	}
	plan.Scope = "full"
	writeExecutionPlan(t, planPath, plan)
	arguments[6] = artifact(planPath).SHA256
	command = exec.CommandContext(t.Context(), binary, arguments...)
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "full executor and oracle contract are unavailable") {
		t.Fatalf("public collection substituted a fixture for full acceptance: %v: %s", err, output)
	}
}

func writeExecutionPlan(t *testing.T, path string, plan searchacceptance.ExecutionPlan) {
	t.Helper()
	content, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

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
