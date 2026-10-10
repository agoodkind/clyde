package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
)

// StatusMetricKind identifies the typed value set in a StatusMetric.
type StatusMetricKind string

const (
	// StatusMetricAbsent means the daemon does not know the value.
	StatusMetricAbsent StatusMetricKind = "absent"
	// StatusMetricInt marks the integer value as set.
	StatusMetricInt StatusMetricKind = "int"
	// StatusMetricFloat marks the float value as set.
	StatusMetricFloat StatusMetricKind = "float"
	// StatusMetricBool marks the boolean value as set.
	StatusMetricBool StatusMetricKind = "bool"
	// StatusMetricText marks the text value as set.
	StatusMetricText StatusMetricKind = "text"
)

// StatusMetric uses Kind to select which of Int, Float, Bool, and Text is set.
type StatusMetric struct {
	Name  string
	Unit  string
	Kind  StatusMetricKind
	Int   int64
	Float float64
	Bool  bool
	Text  string
}

type statusMetricJSON struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
	Unit  string          `json:"unit,omitempty"`
}

// MarshalJSON writes null for an absent value.
func (metric StatusMetric) MarshalJSON() ([]byte, error) {
	var value []byte
	var err error
	switch metric.Kind {
	case StatusMetricInt:
		value, err = json.Marshal(metric.Int)
	case StatusMetricFloat:
		value, err = json.Marshal(metric.Float)
	case StatusMetricBool:
		value, err = json.Marshal(metric.Bool)
	case StatusMetricText:
		value, err = json.Marshal(metric.Text)
	case StatusMetricAbsent:
		value = []byte("null")
	default:
		value = []byte("null")
	}
	if err != nil {
		return nil, fmt.Errorf("encode metric value %s: %w", metric.Name, err)
	}
	encoded, err := json.Marshal(statusMetricJSON{Name: metric.Name, Value: value, Unit: metric.Unit})
	if err != nil {
		return nil, fmt.Errorf("encode metric object %s: %w", metric.Name, err)
	}
	return encoded, nil
}

func absentMetric(name, unit string) StatusMetric {
	return StatusMetric{Name: name, Unit: unit, Kind: StatusMetricAbsent, Int: 0, Float: 0, Bool: false, Text: ""}
}

func intMetric(name string, value int64, unit string) StatusMetric {
	return StatusMetric{Name: name, Unit: unit, Kind: StatusMetricInt, Int: value, Float: 0, Bool: false, Text: ""}
}

func floatMetric(name string, value float64, unit string) StatusMetric {
	return StatusMetric{Name: name, Unit: unit, Kind: StatusMetricFloat, Int: 0, Float: value, Bool: false, Text: ""}
}

func textMetric(name, value string) StatusMetric {
	return StatusMetric{Name: name, Unit: "", Kind: StatusMetricText, Int: 0, Float: 0, Bool: false, Text: value}
}

func knownIntMetric(name string, value int64, unit string, known bool) StatusMetric {
	if !known {
		return absentMetric(name, unit)
	}
	return intMetric(name, value, unit)
}

func knownTextMetric(name, value string, known bool) StatusMetric {
	if !known {
		return absentMetric(name, "")
	}
	return textMetric(name, value)
}

func timeMetric(name string, value time.Time, known bool) StatusMetric {
	if !known || value.IsZero() {
		return absentMetric(name, "")
	}
	return textMetric(name, value.UTC().Format(time.RFC3339))
}

func statusMetricsProto(metrics []StatusMetric) []*clydev1.StatusMetric {
	encoded := make([]*clydev1.StatusMetric, 0, len(metrics))
	for _, metric := range metrics {
		entry := &clydev1.StatusMetric{Name: metric.Name, Unit: metric.Unit}
		switch metric.Kind {
		case StatusMetricInt:
			entry.Value = &clydev1.StatusMetric_IntValue{IntValue: metric.Int}
		case StatusMetricFloat:
			entry.Value = &clydev1.StatusMetric_FloatValue{FloatValue: metric.Float}
		case StatusMetricBool:
			entry.Value = &clydev1.StatusMetric_BoolValue{BoolValue: metric.Bool}
		case StatusMetricText:
			entry.Value = &clydev1.StatusMetric_TextValue{TextValue: metric.Text}
		case StatusMetricAbsent:
		}
		encoded = append(encoded, entry)
	}
	return encoded
}

func statusMetricsFromProto(encoded []*clydev1.StatusMetric) []StatusMetric {
	metrics := make([]StatusMetric, 0, len(encoded))
	for _, entry := range encoded {
		metric := absentMetric(entry.GetName(), entry.GetUnit())
		switch value := entry.GetValue().(type) {
		case *clydev1.StatusMetric_IntValue:
			metric.Kind = StatusMetricInt
			metric.Int = value.IntValue
		case *clydev1.StatusMetric_FloatValue:
			metric.Kind = StatusMetricFloat
			metric.Float = value.FloatValue
		case *clydev1.StatusMetric_BoolValue:
			metric.Kind = StatusMetricBool
			metric.Bool = value.BoolValue
		case *clydev1.StatusMetric_TextValue:
			metric.Kind = StatusMetricText
			metric.Text = value.TextValue
		}
		metrics = append(metrics, metric)
	}
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Name < metrics[j].Name })
	return metrics
}
