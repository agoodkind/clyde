package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/searchacceptance"
)

func runCollection(arguments []string) (err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.collection_command_rejected", "component", "searchacceptance", "concern", "execution", "err", err)
		}
	}()
	flags := flag.NewFlagSet("collect", flag.ContinueOnError)
	var options searchacceptance.ExecutionOptions
	flags.StringVar(&options.Mode, "mode", "", "baseline or candidate")
	flags.StringVar(&options.PlanPath, "plan", "", "absolute saved execution envelope path")
	flags.StringVar(&options.PlanSHA256, "sha256", "", "execution envelope SHA256")
	flags.StringVar(&options.CorpusSnapshot, "corpus", "", "absolute immutable corpus directory")
	flags.StringVar(&options.BatteryPath, "battery", "", "absolute executed battery path")
	flags.StringVar(&options.OraclePath, "oracle", "", "absolute oracle artifact path")
	flags.StringVar(&options.BinaryPath, "binary", "", "absolute explicitly selected workload binary")
	flags.StringVar(&options.BaselineReportPath, "baseline", "", "absolute healthy baseline report path")
	flags.StringVar(&options.ReportPath, "report", "", "new absolute collection report path")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse collection arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("collection accepts no positional arguments")
	}
	result, collectionErr := searchacceptance.CollectExecution(options)
	if result.SavedDiagnosticAssertionsValidated {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return fmt.Errorf("write execution collection result: %w", err)
		}
	}
	return fmt.Errorf("collect saved execution diagnostics: %w", collectionErr)
}
