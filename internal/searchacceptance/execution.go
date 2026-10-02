package searchacceptance

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type executionError struct {
	context string
	cause   error
}

func (err *executionError) Error() string {
	return fmt.Sprintf("%s: %v", err.context, err.cause)
}

func (err *executionError) Unwrap() error {
	return err.cause
}

// ExecutionArtifact binds an existing regular file to its exact bytes.
type ExecutionArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ExecutionPlan binds saved diagnostic evidence without starting a workload.
type ExecutionPlan struct {
	SchemaVersion  int               `json:"schema_version"`
	Mode           string            `json:"mode"`
	Scope          string            `json:"scope"`
	RuntimeRoot    string            `json:"runtime_root"`
	CorpusSnapshot string            `json:"corpus_snapshot"`
	Binary         ExecutionArtifact `json:"binary"`
	Battery        ExecutionArtifact `json:"battery"`
	Oracle         ExecutionArtifact `json:"oracle"`
	Report         ExecutionArtifact `json:"report"`
	BaselineReport ExecutionArtifact `json:"baseline_report"`
	Events         ExecutionArtifact `json:"events"`
	ExecutedPlan   ExecutionArtifact `json:"executed_plan"`
	Lifecycle      ExecutionArtifact `json:"lifecycle"`
	RequiredTests  []string          `json:"required_tests"`
}

// ExecutionOptions supplies explicit public command inputs.
type ExecutionOptions struct {
	Mode, PlanPath, PlanSHA256, CorpusSnapshot, BatteryPath, OraclePath, BinaryPath, BaselineReportPath, ReportPath string
}

// ExecutionCollection separates diagnostic evidence from full acceptance.
type ExecutionCollection struct {
	PlanSHA256                         string   `json:"plan_sha256"`
	Mode                               string   `json:"mode"`
	SavedDiagnosticAssertionsValidated bool     `json:"saved_diagnostic_assertions_validated"`
	AcceptanceComplete                 bool     `json:"acceptance_complete"`
	ReportSHA256                       string   `json:"report_sha256"`
	PassedTests                        []string `json:"passed_tests"`
	MissingRequirements                []string `json:"missing_requirements"`
}

// CollectExecution validates saved evidence and rejects unavailable full execution.
func CollectExecution(options ExecutionOptions) (result ExecutionCollection, err error) {
	if err := validateExecutionInputs(options); err != nil {
		return result, err
	}
	var plan ExecutionPlan
	if err := readExecutionJSON(ExecutionArtifact{options.PlanPath, options.PlanSHA256}, &plan); err != nil {
		return result, err
	}
	if plan.SchemaVersion != 1 || plan.Mode != options.Mode || plan.Scope != "diagnostic" {
		return result, errors.New("only diagnostic envelopes are supported; the full executor and oracle contract are unavailable")
	}
	if plan.CorpusSnapshot != options.CorpusSnapshot || plan.Battery.Path != options.BatteryPath || plan.Oracle.Path != options.OraclePath || plan.Binary.Path != options.BinaryPath {
		return result, errors.New("execution envelope differs from explicit corpus, battery, oracle, or binary inputs")
	}
	if err := validateExecutionRuntime(plan.RuntimeRoot, options.ReportPath, options.CorpusSnapshot); err != nil {
		return result, err
	}
	for _, artifact := range []ExecutionArtifact{plan.Binary, plan.Oracle, plan.ExecutedPlan} {
		if err := verifyExecutionArtifact(artifact); err != nil {
			return result, err
		}
	}
	passed, err := readExecutionTests(plan.Events, plan.RequiredTests)
	if err != nil {
		return result, err
	}
	if err := validateExecutionLifecycle(plan.Lifecycle, plan.ExecutedPlan.SHA256); err != nil {
		return result, err
	}
	batteryBytes, err := readExecutionBytes(plan.Battery)
	if err != nil {
		return result, err
	}
	var battery Battery
	if err := decodeExecutionJSON(strings.NewReader(string(batteryBytes)), &battery); err != nil {
		return result, err
	}
	var report Report
	if err := readExecutionJSON(plan.Report, &report); err != nil {
		return result, err
	}
	if err := ValidateReport(report, battery, batteryBytes); err != nil {
		return result, &executionError{context: "validate saved diagnostic report", cause: err}
	}
	if options.Mode == "candidate" {
		if plan.BaselineReport.Path != options.BaselineReportPath {
			return result, errors.New("baseline report differs from bound envelope")
		}
		var baseline Report
		if err := readExecutionJSON(plan.BaselineReport, &baseline); err != nil {
			return result, err
		}
		if err := ValidateReport(baseline, battery, batteryBytes); err != nil {
			return result, err
		}
		if err := CompareCompatibility(baseline, report); err != nil {
			return result, err
		}
	}
	result = ExecutionCollection{
		PlanSHA256: options.PlanSHA256, Mode: plan.Mode, SavedDiagnosticAssertionsValidated: true, AcceptanceComplete: false, ReportSHA256: plan.Report.SHA256, PassedTests: passed,
		MissingRequirements: []string{"independent raw process and database readbacks", "full executor", "versioned full oracle and scenario report", "complete source admission", "healthy matched baseline and resource acceptance"},
	}
	if err := writeExecutionCollection(options.ReportPath, result); err != nil {
		return result, err
	}
	return result, errors.New("saved artifact hashes, test events, report structure, and lifecycle assertions validated; independent lifecycle proof and full acceptance remain incomplete")
}

func validateExecutionInputs(options ExecutionOptions) error {
	if options.Mode != "baseline" && options.Mode != "candidate" {
		return errors.New("collection mode must be baseline or candidate")
	}
	paths := []string{options.PlanPath, options.CorpusSnapshot, options.BatteryPath, options.OraclePath, options.BinaryPath, options.ReportPath}
	if options.Mode == "candidate" {
		paths = append(paths, options.BaselineReportPath)
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("collection requires explicit clean absolute input and output paths")
		}
	}
	if options.PlanSHA256 == "" {
		return errors.New("collection requires an explicit execution envelope SHA256")
	}
	info, err := os.Stat(options.CorpusSnapshot)
	if err != nil {
		return &executionError{context: "inspect corpus snapshot", cause: err}
	}
	if !info.IsDir() {
		return errors.New("corpus snapshot must be an existing directory")
	}
	if _, err := os.Lstat(options.ReportPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("collection report output must not already exist")
	}
	return nil
}

func validateExecutionRuntime(root, output, corpus string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("execution runtime must be a clean absolute path")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return &executionError{context: "inspect execution runtime", cause: err}
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("execution runtime must be a private non-symlink directory")
	}
	if filepath.Dir(output) != root {
		return errors.New("collection report must be directly inside the private runtime")
	}
	resolvedCorpus, err := filepath.EvalSymlinks(corpus)
	if err != nil {
		return &executionError{context: "resolve immutable corpus", cause: err}
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return &executionError{context: "resolve diagnostic runtime", cause: err}
	}
	relative, err := filepath.Rel(resolvedCorpus, resolvedRoot)
	if err != nil {
		return &executionError{context: "compare corpus and diagnostic runtime", cause: err}
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return errors.New("diagnostic runtime and report must be outside immutable corpus")
	}
	return nil
}

func verifyExecutionArtifact(artifact ExecutionArtifact) error {
	if !filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path {
		return errors.New("execution artifact requires a clean absolute path")
	}
	info, err := os.Lstat(artifact.Path)
	if err != nil {
		return &executionError{context: "inspect execution artifact " + artifact.Path, cause: err}
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("execution artifact %s must be a regular non-symlink file", artifact.Path)
	}
	file, err := os.Open(artifact.Path)
	if err != nil {
		return &executionError{context: "open execution artifact", cause: err}
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return &executionError{context: "hash execution artifact", cause: err}
	}
	if hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return fmt.Errorf("execution artifact %s SHA256 differs", artifact.Path)
	}
	return nil
}

func readExecutionJSON[T ExecutionPlan | executionLifecycle | Report](artifact ExecutionArtifact, result *T) error {
	file, err := openExecutionArtifact(artifact.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if err := decodeExecutionJSON(io.TeeReader(file, hash), result); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return fmt.Errorf("decoded execution artifact %s SHA256 differs", artifact.Path)
	}
	return nil
}

func decodeExecutionJSON[T ExecutionPlan | executionLifecycle | Report | Battery](reader io.Reader, result *T) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return &executionError{context: "decode execution document", cause: err}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("execution document must contain exactly one JSON value")
	}
	return nil
}

func openExecutionArtifact(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("execution artifact requires a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, &executionError{context: "inspect execution artifact", cause: err}
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("execution artifact must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, &executionError{context: "open execution artifact", cause: err}
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, &executionError{context: "verify opened execution artifact", cause: errors.Join(errors.New("execution artifact changed while opening"), err, file.Close())}
	}
	return file, nil
}

func readExecutionBytes(artifact ExecutionArtifact) ([]byte, error) {
	file, err := openExecutionArtifact(artifact.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, &executionError{context: "read bound execution artifact", cause: err}
	}
	if Digest(content) != artifact.SHA256 {
		return nil, errors.New("consumed execution artifact SHA256 differs")
	}
	return content, nil
}

type executionTestAction string

const (
	executionTestRun  executionTestAction = "run"
	executionTestPass executionTestAction = "pass"
	executionTestFail executionTestAction = "fail"
	executionTestSkip executionTestAction = "skip"
)

type executionTestEvent struct {
	Action executionTestAction `json:"Action"`
	Test   string              `json:"Test"`
}

func readExecutionTests(artifact ExecutionArtifact, required []string) ([]string, error) {
	if len(required) == 0 {
		return nil, errors.New("execution requires an explicit nonempty test inventory")
	}
	states := make(map[string]string, len(required))
	for _, name := range required {
		if name == "" {
			return nil, errors.New("required test name is empty")
		}
		if _, exists := states[name]; exists {
			return nil, errors.New("required test inventory repeats a name")
		}
		states[name] = "missing"
	}
	file, err := openExecutionArtifact(artifact.Path)
	if err != nil {
		return nil, &executionError{context: "open executed test events", cause: err}
	}
	defer file.Close()
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(file, hash))
	scanner.Buffer(make([]byte, 65536), 16777216)
	for scanner.Scan() {
		var event executionTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, &executionError{context: "decode actual test event", cause: err}
		}
		switch event.Action {
		case executionTestFail, executionTestSkip:
			return nil, fmt.Errorf("executed test event rejected: %s %s", event.Action, event.Test)
		case executionTestRun:
			if _, found := states[event.Test]; !found {
				continue
			}
			if states[event.Test] != "missing" {
				return nil, fmt.Errorf("required test %s started more than once", event.Test)
			}
			states[event.Test] = "running"
		case executionTestPass:
			if _, found := states[event.Test]; !found {
				continue
			}
			if states[event.Test] != "running" {
				return nil, fmt.Errorf("required test %s passed without one start", event.Test)
			}
			states[event.Test] = "passed"
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, &executionError{context: "read actual test events", cause: err}
	}
	if hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return nil, errors.New("consumed test event SHA256 differs")
	}
	for name, state := range states {
		if state != "passed" {
			return nil, fmt.Errorf("required test %s is %s", name, state)
		}
	}
	passed := slices.Clone(required)
	slices.Sort(passed)
	return passed, nil
}

type executionLifecycle struct {
	ExecutedPlanSHA256           string         `json:"executed_plan_sha256"`
	ExitStatus                   *int           `json:"exit_status"`
	GuardJoined                  bool           `json:"guard_joined"`
	ChildrenJoined               bool           `json:"children_joined"`
	IndependentPIDsAbsent        bool           `json:"independent_pids_absent"`
	RegisteredDatabasesAbsent    bool           `json:"registered_databases_absent"`
	PriorDatabaseAbsenceVerified bool           `json:"prior_database_absence_verified"`
	ProductionBefore             executionStops `json:"production_before"`
	ProductionAfter              executionStops `json:"production_after"`
}

type executionStops struct {
	ClydeSearch    bool `json:"clyde_search_stopped"`
	ClydeIngestion bool `json:"clyde_ingestion_stopped"`
	LMSSearch      bool `json:"lms_search_stopped"`
	LMSIngestion   bool `json:"lms_ingestion_stopped"`
}

func validateExecutionLifecycle(artifact ExecutionArtifact, executedSHA256 string) error {
	var proof executionLifecycle
	if err := readExecutionJSON(artifact, &proof); err != nil {
		return err
	}
	if proof.ExecutedPlanSHA256 != executedSHA256 || proof.ExitStatus == nil || *proof.ExitStatus != 0 {
		return errors.New("execution lifecycle has a different plan or unsuccessful command")
	}
	if !proof.GuardJoined || !proof.ChildrenJoined || !proof.IndependentPIDsAbsent || !proof.RegisteredDatabasesAbsent || !proof.PriorDatabaseAbsenceVerified {
		return errors.New("saved lifecycle assertions omit joins, PID absence, or database cleanup")
	}
	for _, stops := range []executionStops{proof.ProductionBefore, proof.ProductionAfter} {
		if !stops.ClydeSearch || !stops.ClydeIngestion || !stops.LMSSearch || !stops.LMSIngestion {
			return errors.New("saved lifecycle assertions omit a production stop")
		}
	}
	return nil
}

func writeExecutionCollection(path string, collection ExecutionCollection) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return &executionError{context: "create exclusive collection report", cause: err}
	}
	encodeErr := json.NewEncoder(file).Encode(collection)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(encodeErr, syncErr, closeErr); err != nil {
		return &executionError{context: "persist collection report", cause: err}
	}
	return nil
}
