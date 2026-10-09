package daemon_test

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/daemon"
)

func statusProofMetric(metrics []daemon.StatusMetric, name string) (daemon.StatusMetric, bool) {
	for _, metric := range metrics {
		if metric.Name == name {
			return metric, true
		}
	}
	return daemon.StatusMetric{Name: name, Unit: "", Kind: daemon.StatusMetricAbsent, Int: 0, Float: 0, Bool: false, Text: ""}, false
}

func statusProofKnownInt(metrics []daemon.StatusMetric, name string) (int64, bool) {
	metric, found := statusProofMetric(metrics, name)
	return metric.Int, found && metric.Kind == daemon.StatusMetricInt
}

func statusProofText(metrics []daemon.StatusMetric, name string) string {
	metric, found := statusProofMetric(metrics, name)
	if !found || metric.Kind != daemon.StatusMetricText {
		return ""
	}
	return metric.Text
}

func statusProofInt(t *testing.T, step string, metrics []daemon.StatusMetric, name string) int64 {
	t.Helper()
	value, known := statusProofKnownInt(metrics, name)
	if !known {
		metric, found := statusProofMetric(metrics, name)
		t.Errorf("step=%s metric=%s found=%t kind=%s want_kind=%s", step, name, found, metric.Kind, daemon.StatusMetricInt)
	}
	return value
}

func statusProofNumber(t *testing.T, step string, metrics []daemon.StatusMetric, name string) float64 {
	t.Helper()
	metric, found := statusProofMetric(metrics, name)
	switch {
	case found && metric.Kind == daemon.StatusMetricFloat:
		return metric.Float
	case found && metric.Kind == daemon.StatusMetricInt:
		return float64(metric.Int)
	default:
		t.Errorf("step=%s metric=%s found=%t kind=%s want_number", step, name, found, metric.Kind)
		return 0
	}
}

func assertStatusProofTexts(t *testing.T, step string, metrics []daemon.StatusMetric, names ...string) {
	t.Helper()
	for _, name := range names {
		if statusProofText(metrics, name) == "" {
			metric, found := statusProofMetric(metrics, name)
			t.Errorf("step=%s metric=%s found=%t kind=%s want_text", step, name, found, metric.Kind)
		}
	}
}

func assertStatusProofUnknown(t *testing.T, step string, metrics []daemon.StatusMetric, names ...string) {
	t.Helper()
	for _, name := range names {
		metric, found := statusProofMetric(metrics, name)
		if !found || metric.Kind != daemon.StatusMetricAbsent {
			t.Errorf("step=%s metric=%s found=%t kind=%s want_kind=%s", step, name, found, metric.Kind, daemon.StatusMetricAbsent)
		}
	}
}

func assertStatusProofMissing(t *testing.T, step string, metrics []daemon.StatusMetric, names ...string) {
	t.Helper()
	for _, name := range names {
		if metric, found := statusProofMetric(metrics, name); found {
			t.Errorf("step=%s metric=%s kind=%s want_missing", step, name, metric.Kind)
		}
	}
}

func statusProofMetricsWithPrefix(metrics []daemon.StatusMetric, prefixes ...string) []daemon.StatusMetric {
	selected := make([]daemon.StatusMetric, 0)
	for _, metric := range metrics {
		for _, prefix := range prefixes {
			if strings.HasPrefix(metric.Name, prefix) {
				selected = append(selected, metric)
				break
			}
		}
	}
	return selected
}
