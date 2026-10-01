package searchacceptance

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
)

// Measurement compares one recorded statistic under the same workload.
type Measurement struct {
	Name       string  `json:"name"`
	Unit       string  `json:"unit"`
	Baseline   float64 `json:"baseline"`
	Candidate  float64 `json:"candidate"`
	Regression bool    `json:"regression"`
}

// Comparison reports measured changes without asserting unmeasured acceptance.
type Comparison struct {
	WorkloadCompatible bool          `json:"workload_compatible"`
	PerformancePassed  bool          `json:"performance_passed"`
	AcceptanceComplete bool          `json:"acceptance_complete"`
	BaselineRevision   string        `json:"baseline_revision"`
	CandidateRevision  string        `json:"candidate_revision"`
	Measurements       []Measurement `json:"measurements"`
}

// ReadComparison rejects invalid reports and measured latency or RSS regression.
// Full acceptance also requires the source oracle and lifecycle evidence.
func ReadComparison(baselinePath, candidatePath, batteryPath string) (comparison Comparison, err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.performance_rejected", "component", "searchacceptance", "concern", "comparison", "err", err)
		}
	}()
	baseline, _, err := ReadReport(baselinePath, batteryPath)
	if err != nil {
		return Comparison{}, fmt.Errorf("read baseline comparison: %w", err)
	}
	candidate, _, err := ReadReport(candidatePath, batteryPath)
	if err != nil {
		return Comparison{}, fmt.Errorf("read candidate comparison: %w", err)
	}
	if err := CompareCompatibility(baseline, candidate); err != nil {
		return Comparison{}, err
	}
	baselineSamples := latencySamples(baseline)
	candidateSamples := latencySamples(candidate)
	if len(baselineSamples) != len(candidateSamples) {
		return Comparison{}, errors.New("baseline and candidate latency categories differ")
	}
	result := Comparison{
		WorkloadCompatible: true, PerformancePassed: true,
		BaselineRevision: baseline.BuildRevision, CandidateRevision: candidate.BuildRevision,
		AcceptanceComplete: false, Measurements: nil,
	}
	names := make([]string, 0, len(baselineSamples))
	for name := range baselineSamples {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		baselineValues := baselineSamples[name]
		candidateValues := candidateSamples[name]
		if len(baselineValues) != len(candidateValues) {
			return result, fmt.Errorf("measurement %s has different baseline and candidate sample counts", name)
		}
		appendLatencyMeasurements(&result, name, baselineValues, candidateValues)
	}
	appendMemoryMeasurements(&result, baseline.Resources, candidate.Resources)
	if !result.PerformancePassed {
		return result, errors.New("candidate latency or peak RSS exceeds the matched baseline")
	}
	return result, nil
}

func latencySamples(report Report) map[string][]float64 {
	samples := make(map[string][]float64)
	for _, traversal := range report.Traversals {
		state := "warm"
		if traversal.Cold {
			state = "cold"
		}
		prefix := traversal.QueryID + "/" + state + "/"
		var total float64
		for index, page := range traversal.Pages {
			category := "later_page"
			if index == 0 {
				category = "first_page"
			}
			samples[prefix+category] = append(samples[prefix+category], page.ElapsedMS)
			total += page.ElapsedMS
		}
		samples[prefix+"full_traversal"] = append(samples[prefix+"full_traversal"], total)
	}
	return samples
}

func appendLatencyMeasurements(result *Comparison, name string, baseline, candidate []float64) {
	slices.Sort(baseline)
	slices.Sort(candidate)
	for _, quantile := range []struct {
		name  string
		value float64
	}{{"p50", 0.50}, {"p95", 0.95}, {"maximum", 1}} {
		index := int(math.Ceil(float64(len(baseline))*quantile.value)) - 1
		appendMeasurement(result, name+"/"+quantile.name, "milliseconds", baseline[index], candidate[index])
	}
}

func appendMemoryMeasurements(result *Comparison, baseline, candidate Resources) {
	for _, measurement := range []struct {
		name      string
		baseline  int64
		candidate int64
	}{
		{"clyde_peak_rss", baseline.ClydePeakRSSBytes, candidate.ClydePeakRSSBytes},
		{"model_peak_rss", baseline.ModelPeakRSSBytes, candidate.ModelPeakRSSBytes},
		{"milvus_peak_rss", baseline.MilvusPeakRSSBytes, candidate.MilvusPeakRSSBytes},
		{"total_peak_rss", baseline.TotalPeakRSSBytes, candidate.TotalPeakRSSBytes},
	} {
		appendMeasurement(result, measurement.name, "bytes", float64(measurement.baseline), float64(measurement.candidate))
	}
}

func appendMeasurement(result *Comparison, name, unit string, baseline, candidate float64) {
	regression := candidate > baseline
	result.Measurements = append(result.Measurements, Measurement{
		Name: name, Unit: unit, Baseline: baseline, Candidate: candidate, Regression: regression,
	})
	if regression {
		result.PerformancePassed = false
	}
}
