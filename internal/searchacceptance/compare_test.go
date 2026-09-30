package searchacceptance_test

import (
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/searchacceptance"
)

func TestReadComparisonRejectsMeasuredRegression(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*searchacceptance.Report)
	}{
		{"cold first page", func(report *searchacceptance.Report) { report.Traversals[0].Pages[0].ElapsedMS++ }},
		{"warm later page", func(report *searchacceptance.Report) { report.Traversals[1].Pages[1].ElapsedMS++ }},
		{"clyde memory", func(report *searchacceptance.Report) { report.Resources.ClydePeakRSSBytes++ }},
		{"model memory", func(report *searchacceptance.Report) { report.Resources.ModelPeakRSSBytes++ }},
		{"milvus memory", func(report *searchacceptance.Report) { report.Resources.MilvusPeakRSSBytes++ }},
		{"total memory", func(report *searchacceptance.Report) { report.Resources.TotalPeakRSSBytes++ }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			baselinePath, batteryPath, report := savedReport(t)
			candidatePath := filepath.Join(filepath.Dir(baselinePath), "candidate.json")
			writeJSON(t, candidatePath, report)
			control, err := searchacceptance.ReadComparison(baselinePath, candidatePath, batteryPath)
			if err != nil || !control.PerformancePassed || control.AcceptanceComplete {
				t.Fatalf("matched control comparison failed: %+v, %v", control, err)
			}
			testCase.mutate(&report)
			writeJSON(t, candidatePath, report)
			result, err := searchacceptance.ReadComparison(baselinePath, candidatePath, batteryPath)
			if err == nil || result.PerformancePassed {
				t.Fatal("measured regression passed")
			}
			found := false
			for _, measurement := range result.Measurements {
				if measurement.Regression && measurement.Candidate > measurement.Baseline {
					found = true
				}
			}
			if !found {
				t.Fatal("comparison omitted the regressed measurement")
			}
		})
	}
}

func TestReadComparisonRejectsDifferentSampleCounts(t *testing.T) {
	baselinePath, batteryPath, report := savedReport(t)
	candidatePath := filepath.Join(filepath.Dir(baselinePath), "candidate.json")
	report.Traversals = append(report.Traversals, report.Traversals[1])
	writeJSON(t, candidatePath, report)
	if _, err := searchacceptance.ReadComparison(baselinePath, candidatePath, batteryPath); err == nil {
		t.Fatal("unequal sample counts passed")
	}
}
