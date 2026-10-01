package searchacceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/searchacceptance"
)

func TestReadReportRejectsBrokenTraversals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*searchacceptance.Report)
	}{
		{"repeated identity", func(report *searchacceptance.Report) { report.Traversals[0].Pages[1].OccurrenceIDs[0] = "a" }},
		{"missing identity", func(report *searchacceptance.Report) {
			report.Traversals[0].Pages = report.Traversals[0].Pages[:1]
			report.Traversals[0].Pages[0].HasMore = false
		}},
		{"short nonterminal page", func(report *searchacceptance.Report) { report.Traversals[0].Pages[0].OccurrenceIDs = []string{"a"} }},
		{"timeout", func(report *searchacceptance.Report) { report.Traversals[0].Pages[0].ElapsedMS = 30001 }},
		{"missing warm traversal", func(report *searchacceptance.Report) { report.Traversals = report.Traversals[:1] }},
		{"partial success", func(report *searchacceptance.Report) { report.Traversals[0].Error = "store unavailable" }},
		{"unfinished unchanged pass", func(report *searchacceptance.Report) { report.Ingestion.UnchangedPassCompleted = false }},
		{"missing memory measurement", func(report *searchacceptance.Report) { report.Resources.ModelPeakRSSBytes = 0 }},
		{"negative stage time", func(report *searchacceptance.Report) { report.Ingestion.EmbeddingMS = -1 }},
		{"missing selected counts", func(report *searchacceptance.Report) { report.Ingestion.SelectedCounts = nil }},
		{"impossible total memory", func(report *searchacceptance.Report) { report.Resources.TotalPeakRSSBytes = 1 }},
		{"unchanged embedding request", func(report *searchacceptance.Report) { report.Ingestion.UnchangedPassEmbeddingRequests = 1 }},
		{"battery mismatch", func(report *searchacceptance.Report) {
			report.BatteryDigest = searchacceptance.Digest([]byte("different battery"))
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reportPath, batteryPath, report := savedReport(t)
			if _, _, err := searchacceptance.ReadReport(reportPath, batteryPath); err != nil {
				t.Fatalf("complete control report failed: %v", err)
			}
			testCase.mutate(&report)
			writeJSON(t, reportPath, report)
			if _, _, err := searchacceptance.ReadReport(reportPath, batteryPath); err == nil {
				t.Fatal("broken traversal was accepted")
			}
		})
	}
}

func TestReadReportRejectsChangedTraversalOrder(t *testing.T) {
	reportPath, batteryPath, report := savedReport(t)
	report.Traversals = append(report.Traversals, searchacceptance.Traversal{
		QueryID: "query", Cold: false, Total: 3,
		Pages: []searchacceptance.Page{
			{OccurrenceIDs: []string{"a", "b"}, ElapsedMS: 1, HasMore: true},
			{OccurrenceIDs: []string{"c"}, ElapsedMS: 1, HasMore: false},
		},
	})
	writeJSON(t, reportPath, report)
	if _, _, err := searchacceptance.ReadReport(reportPath, batteryPath); err != nil {
		t.Fatalf("complete repeat failed: %v", err)
	}
	report.Traversals[2].Pages[0].OccurrenceIDs = []string{"b", "a"}
	writeJSON(t, reportPath, report)
	if _, _, err := searchacceptance.ReadReport(reportPath, batteryPath); err == nil {
		t.Fatal("unstable traversal order was accepted")
	}
}

func TestReportCompatibilityRejectsChangedWorkload(t *testing.T) {
	reportPath, batteryPath, baseline := savedReport(t)
	candidate := baseline
	candidate.BuildRevision = "candidate-revision"
	writeJSON(t, reportPath, candidate)
	candidate, _, err := searchacceptance.ReadReport(reportPath, batteryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := searchacceptance.CompareCompatibility(baseline, candidate); err != nil {
		t.Fatalf("compatible candidate failed: %v", err)
	}
	candidate.ModelDescriptor.Revision = "different-model"
	if err := searchacceptance.CompareCompatibility(baseline, candidate); err == nil {
		t.Fatal("different model revision was accepted")
	}
}

func savedReport(t *testing.T) (string, string, searchacceptance.Report) {
	t.Helper()
	root := t.TempDir()
	batteryPath := filepath.Join(root, "battery.json")
	reportPath := filepath.Join(root, "report.json")
	battery := searchacceptance.Battery{Concurrency: 1, TimeoutMS: 30000, Entries: []searchacceptance.Query{
		{ID: "query", Query: "storage", PageSize: 2, ExpectedOccurrenceIDs: []string{"a", "b", "c"}, ExpectedTotal: 3},
	}}
	batteryBytes := writeJSON(t, batteryPath, battery)
	traversal := searchacceptance.Traversal{QueryID: "query", Cold: true, Total: 3, Pages: []searchacceptance.Page{
		{OccurrenceIDs: []string{"a", "b"}, ElapsedMS: 2, HasMore: true},
		{OccurrenceIDs: []string{"c"}, ElapsedMS: 1, HasMore: false},
	}}
	warm := searchacceptance.Traversal{QueryID: "query", Cold: false, Total: 3, Pages: []searchacceptance.Page{
		{OccurrenceIDs: []string{"a", "b"}, ElapsedMS: 1, HasMore: true},
		{OccurrenceIDs: []string{"c"}, ElapsedMS: 1, HasMore: false},
	}}
	report := searchacceptance.Report{
		SchemaVersion: searchacceptance.SchemaVersion, BuildRevision: "baseline-revision", CorpusDigest: searchacceptance.Digest([]byte("source corpus")),
		HardwareIdentity: "isolated-mac", ModelDescriptor: searchacceptance.Model{Name: "model", Revision: "revision", Dimension: 4096, Normalization: "l2"},
		BatteryDigest: searchacceptance.Digest(batteryBytes), Concurrency: 1, Traversals: []searchacceptance.Traversal{traversal, warm},
		Ingestion: searchacceptance.Ingestion{
			Completed: true, UnchangedPassCompleted: true, SearchableCompletionMS: 10,
			SelectedCounts: map[string]int64{"claude/chat": 3}, ExcludedCounts: map[string]int64{},
		},
		Resources: searchacceptance.Resources{
			ClydePeakRSSBytes: 100, ModelPeakRSSBytes: 200,
			MilvusPeakRSSBytes: 300, TotalPeakRSSBytes: 500, CompactedBytes: 1000,
		},
	}
	writeJSON(t, reportPath, report)
	return reportPath, batteryPath, report
}

func writeJSON[T searchacceptance.Report | searchacceptance.Battery](t *testing.T, path string, value T) []byte {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return content
}
