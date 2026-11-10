package clispec

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"

	daemonsvc "goodkind.io/clyde/internal/daemon"
)

func TestDaemonStatusSincePreparesMetricsWindow(t *testing.T) {
	t.Parallel()
	op := daemonStatusOp()
	if !slices.ContainsFunc(op.Params, func(param Param[daemonStatusInput]) bool {
		return param.Canonical == "since" && param.Kind == KindString
	}) {
		t.Fatal("daemon status must expose --since")
	}
	payload, err := op.Prepare(daemonStatusInput{TimeoutSeconds: 3, Since: "1h"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if payload.Since != time.Hour {
		t.Fatalf("since = %s, want %s", payload.Since, time.Hour)
	}
}

func TestDaemonMetricsStatusOutputHasOnlyStableHistoricalKeys(t *testing.T) {
	t.Parallel()
	payload := daemonMetricsStatusOutput{
		Window: daemonsvc.MetricsWindow{}, Coverage: daemonsvc.MetricsCoverage{},
		Metrics: daemonsvc.MetricsValues{}, TimeBreakdown: daemonsvc.MetricsTimeBreakdown{},
		UnattributedDurationMS: nil, Warnings: []string{},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var contract struct {
		Window                 json.RawMessage `json:"window"`
		Coverage               json.RawMessage `json:"coverage"`
		Metrics                json.RawMessage `json:"metrics"`
		TimeBreakdown          json.RawMessage `json:"time_breakdown"`
		UnattributedDurationMS json.RawMessage `json:"unattributed_duration_ms"`
		Warnings               json.RawMessage `json:"warnings"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		t.Fatalf("Decode stable contract: %v body=%s", err, body)
	}
	if string(contract.Warnings) != "[]" || string(contract.UnattributedDurationMS) != "null" {
		t.Fatalf("warnings or unavailable number = %s, want [] and null", body)
	}
}
