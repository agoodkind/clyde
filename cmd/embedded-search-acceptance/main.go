// Command embedded-search-acceptance validates saved isolated search reports.
package main

import (
	"context"
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
	if len(os.Args) > 1 && os.Args[1] == "collect" {
		return runCollection(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-snapshot" {
		return runSnapshot(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "export-sources" {
		return runSourceExport(os.Args[2:])
	}
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

func runSnapshot(arguments []string) (err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.snapshot_command_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	flags := flag.NewFlagSet("verify-snapshot", flag.ContinueOnError)
	root := flags.String("root", "", "absolute frozen snapshot root")
	manifest := flags.String("manifest", "", "absolute SHA-256 manifest path")
	digest := flags.String("sha256", "", "approved manifest SHA-256 digest")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse snapshot verification arguments: %w", err)
	}
	if *root == "" || *manifest == "" || *digest == "" || flags.NArg() != 0 {
		return fmt.Errorf("snapshot root, manifest, and approved SHA-256 digest are required")
	}
	result, err := searchacceptance.VerifySnapshot(context.Background(), *root, *manifest, *digest)
	if err != nil {
		return fmt.Errorf("verify frozen snapshot: %w", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return fmt.Errorf("write snapshot verification: %w", err)
	}
	return nil
}
