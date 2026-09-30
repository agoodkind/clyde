package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/searchacceptance"
)

func runBatteryInspection(arguments []string) error {
	flags := flag.NewFlagSet("inspect-battery", flag.ContinueOnError)
	path := flags.String("battery", "", "absolute original frozen query battery")
	digest := flags.String("sha256", "", "expected original battery SHA-256")
	if err := flags.Parse(arguments); err != nil {
		slog.Warn("search.acceptance.battery_arguments_rejected", "component", "searchacceptance", "concern", "battery", "err", err)
		return fmt.Errorf("parse battery inspection arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("battery inspection rejects positional arguments")
	}
	inspection, err := searchacceptance.InspectFrozenBattery(*path, *digest, originalBatteryConstraints())
	if err != nil {
		slog.Warn("search.acceptance.battery_inspection_failed", "component", "searchacceptance", "concern", "battery", "err", err)
		return fmt.Errorf("inspect original battery: %w", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(inspection); err != nil {
		slog.Warn("search.acceptance.battery_output_failed", "component", "searchacceptance", "concern", "battery", "err", err)
		return fmt.Errorf("write battery inspection: %w", err)
	}
	return nil
}

func originalBatteryConstraints() searchacceptance.BatteryConstraints {
	queries := make([]string, 51)
	for index := range queries {
		queries[index] = fmt.Sprintf("q%03d", index+1)
	}
	return searchacceptance.BatteryConstraints{
		SchemaVersion: 1,
		QuerySetID:    "shared-search-query-set-20260928T0730Z",
		QueryIDs:      queries,
		Scenarios: []searchacceptance.ScenarioConstraint{
			{ID: "s001", Kind: "missing_artifact", RequireReferences: true},
			{ID: "s002", Kind: "concurrent_append", RequireReferences: true},
			{ID: "s003", Kind: "unchanged_second_pass", RequireReferences: false},
		},
	}
}
