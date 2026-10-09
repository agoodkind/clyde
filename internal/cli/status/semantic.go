package status

import (
	"math"
	"strings"
	"time"

	lmstatus "goodkind.io/lm-semantic-search/status"

	daemonsvc "goodkind.io/clyde/internal/daemon"
)

const (
	groupSemantic = "semantic"
	unitAttempts  = "attempts"

	metricGroupSeparator = "."
)

func optionalInt(group, name string, value *int64, unit string) lmstatus.Field {
	field := lmstatus.Field{Group: group, Name: name, Unit: unit, Value: lmstatus.Value{}, NoDelta: false}
	if value != nil {
		field.Value = lmstatus.Int(*value)
	}
	return field
}

func optionalText(group, name string, value *string) lmstatus.Field {
	field := lmstatus.Field{Group: group, Name: name, Unit: "", Value: lmstatus.Value{}, NoDelta: false}
	if value != nil {
		field.Value = lmstatus.Text(*value)
	}
	return field
}

func optionalBool(group, name string, value *bool) lmstatus.Field {
	field := lmstatus.Field{Group: group, Name: name, Unit: "", Value: lmstatus.Value{}, NoDelta: false}
	if value != nil {
		field.Value = lmstatus.Bool(*value)
	}
	return field
}

func optionalTime(group, name string, unix *int64) lmstatus.Field {
	field := lmstatus.Field{Group: group, Name: name, Unit: "", Value: lmstatus.Value{}, NoDelta: true}
	if unix != nil {
		field.Value = lmstatus.Text(time.Unix(*unix, 0).UTC().Format(time.RFC3339))
	}
	return field
}

func metricFields(metrics []daemonsvc.StatusMetric) []lmstatus.Field {
	fields := make([]lmstatus.Field, 0, len(metrics))
	for _, metric := range metrics {
		group, _, _ := strings.Cut(metric.Name, metricGroupSeparator)
		field := lmstatus.Field{Group: group, Name: metric.Name, Unit: metric.Unit, Value: lmstatus.Value{}, NoDelta: false}
		switch metric.Kind {
		case daemonsvc.StatusMetricInt:
			field.Value = lmstatus.Int(metric.Int)
		case daemonsvc.StatusMetricFloat:
			field.Value = lmstatus.Float(metric.Float)
		case daemonsvc.StatusMetricBool:
			field.Value = lmstatus.Bool(metric.Bool)
		case daemonsvc.StatusMetricText:
			field.Value = lmstatus.Text(metric.Text)
		case daemonsvc.StatusMetricAbsent:
		}
		fields = append(fields, field)
	}
	return fields
}

func semanticFields(runtime *daemonsvc.RuntimeStatus) []lmstatus.Field {
	var ingestionEnabled, searchEnabled *bool
	var backend, connection *string
	var attempts, nextRetry *int64
	var detail, process []daemonsvc.StatusMetric
	if runtime != nil {
		semantic := runtime.Semantic
		ingestionEnabled = &semantic.IngestionEnabled
		searchEnabled = &semantic.SearchEnabled
		backendText := string(semantic.Backend)
		backend = &backendText
		connectionText := string(semantic.Connection)
		connection = &connectionText
		attemptCount := int64(math.MaxInt64)
		if semantic.Attempts < math.MaxInt64 {
			attemptCount = int64(semantic.Attempts)
		}
		attempts = &attemptCount
		nextRetry = semantic.NextRetryUnix
		detail = semantic.Detail
		process = runtime.Process
	}
	fields := []lmstatus.Field{
		optionalBool(groupSemantic, "semantic.ingestion_enabled", ingestionEnabled),
		optionalBool(groupSemantic, "semantic.search_enabled", searchEnabled),
		optionalText(groupSemantic, "semantic.backend", backend),
		optionalText(groupSemantic, "semantic.connection", connection),
		optionalInt(groupSemantic, "semantic.attempts", attempts, unitAttempts),
		optionalTime(groupSemantic, "semantic.next_retry", nextRetry),
	}
	fields = append(fields, metricFields(detail)...)
	return append(fields, metricFields(process)...)
}
