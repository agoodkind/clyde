package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/searchacceptance"
)

type sourceExportOptions struct {
	root         string
	originalHome string
	manifest     string
	digest       string
	settings     string
	output       string
	worker       bool
}

type sourceExportSettings struct {
	Semantic        config.ConversationSemanticConfig `json:"semantic"`
	Model           searchacceptance.Model            `json:"model"`
	ArtifactSettled bool                              `json:"artifact_settled"`
}

func runSourceExport(arguments []string) (err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.source_command_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	options, err := parseSourceExportOptions(arguments)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !options.worker {
		return runSourceExportChild(ctx, options, arguments)
	}
	return writeSourceExport(ctx, options)
}

func parseSourceExportOptions(arguments []string) (sourceExportOptions, error) {
	var options sourceExportOptions
	flags := flag.NewFlagSet("export-sources", flag.ContinueOnError)
	flags.StringVar(&options.root, "root", "", "absolute frozen snapshot root")
	flags.StringVar(&options.originalHome, "original-home", "", "original home recorded in the frozen index")
	flags.StringVar(&options.manifest, "manifest", "", "absolute approved source manifest path")
	flags.StringVar(&options.digest, "sha256", "", "approved manifest SHA-256 digest")
	flags.StringVar(&options.settings, "settings", "", "absolute source selection and model JSON path")
	flags.StringVar(&options.output, "output", "", "absolute new source JSONL artifact path")
	flags.BoolVar(&options.worker, "worker", false, "run inside the isolated source process")
	if err := flags.Parse(arguments); err != nil {
		slog.Warn("search.acceptance.source_arguments_failed", "component", "searchacceptance", "concern", "source", "err", err)
		return options, fmt.Errorf("parse source export arguments: %w", err)
	}
	if options.digest == "" || flags.NArg() != 0 {
		return options, errors.New("approved source digest and named source arguments are required")
	}
	for _, path := range []string{options.root, options.originalHome, options.manifest, options.settings, options.output} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return options, errors.New("source roots and artifact paths must be clean absolute paths")
		}
	}
	if err := validateSourceOutput(options); err != nil {
		return options, err
	}
	return options, nil
}

func validateSourceOutput(options sourceExportOptions) (err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.source_output_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	root, err := filepath.EvalSymlinks(options.root)
	if err != nil {
		return fmt.Errorf("resolve frozen source root: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(options.output))
	if err != nil {
		return fmt.Errorf("resolve source output directory: %w", err)
	}
	relative, err := filepath.Rel(root, parent)
	if err != nil {
		return fmt.Errorf("compare source output directory: %w", err)
	}
	if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("source output must be outside the frozen snapshot")
	}
	return nil
}

func runSourceExportChild(ctx context.Context, options sourceExportOptions, arguments []string) (err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "search.acceptance.source_child_failed", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve source exporter executable: %w", err)
	}
	childArguments := append([]string{"export-sources"}, arguments...)
	childArguments = append(childArguments, "--worker")
	command := exec.CommandContext(ctx, executable)
	command.Args = append(command.Args, childArguments...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + options.originalHome, "TMPDIR=" + os.TempDir()}
	for _, provider := range []conversation.Provider{conversation.ProviderCursor, conversation.ProviderZed} {
		environment, err := searchacceptance.FrozenProviderEnvironment(options.root, provider)
		if err != nil {
			return fmt.Errorf("resolve frozen provider environment: %w", err)
		}
		command.Env = append(command.Env, environment.Name+"="+environment.Value)
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run isolated source exporter: %w", err)
	}
	return nil
}

func writeSourceExport(ctx context.Context, options sourceExportOptions) (err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "search.acceptance.source_output_failed", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	settings, err := readSourceExportSettings(options.settings)
	if err != nil {
		return err
	}
	verification, err := searchacceptance.VerifySnapshot(ctx, options.root, options.manifest, options.digest)
	if err != nil {
		return fmt.Errorf("verify frozen source snapshot: %w", err)
	}
	files, err := searchacceptance.SnapshotManifestFiles(options.root, options.manifest, options.digest)
	if err != nil {
		return fmt.Errorf("resolve approved source files: %w", err)
	}
	output, err := os.OpenFile(options.output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create new source artifact: %w", err)
	}
	request := searchacceptance.FrozenSourceRequest{
		SnapshotRoot: options.root, OriginalHome: options.originalHome, ManifestFiles: files,
		ManifestDigest: verification.ManifestDigest, Verification: verification,
		Semantic: settings.Semantic, Model: settings.Model, ArtifactSettled: settings.ArtifactSettled,
	}
	summary, exportErr := searchacceptance.ExportFrozenSources(ctx, request, output)
	if err := errors.Join(exportErr, output.Close()); err != nil {
		return fmt.Errorf("complete source artifact: %w", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		return fmt.Errorf("write source summary: %w", err)
	}
	return nil
}

func readSourceExportSettings(path string) (settings sourceExportSettings, err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.source_settings_failed", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	file, err := os.Open(path)
	if err != nil {
		return settings, fmt.Errorf("open source settings: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, fmt.Errorf("decode source settings: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return settings, errors.New("source settings must contain one JSON object")
	}
	return settings, nil
}
