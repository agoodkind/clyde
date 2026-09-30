// Command embedded-search-acceptance validates saved isolated search reports.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/clyde/internal/searchacceptance"
)

func main() {
	if err := run(); err != nil {
		slog.Error("search.acceptance.failed", "component", "searchacceptance", "concern", "command", "err", err)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.comparison_rejected", "component", "searchacceptance", "concern", "report", "err", err)
		}
	}()
	batteryPath := flag.String("battery", "", "absolute frozen query battery path")
	baselinePath := flag.String("baseline", "", "absolute baseline report path")
	candidatePath := flag.String("candidate", "", "absolute candidate report path")
	flag.Parse()
	if *batteryPath == "" || *baselinePath == "" || *candidatePath == "" || flag.NArg() != 0 {
		return fmt.Errorf("battery, baseline, and candidate report paths are required")
	}
	for _, path := range []string{*batteryPath, *baselinePath, *candidatePath} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("report and battery paths must be absolute")
		}
	}
	result, comparisonErr := searchacceptance.ReadComparison(*baselinePath, *candidatePath, *batteryPath)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return fmt.Errorf("write report validation: %w", err)
	}
	if comparisonErr != nil {
		return fmt.Errorf("compare search reports: %w", comparisonErr)
	}
	return nil
}
