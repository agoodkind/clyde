package daemon

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

type metricsMessage string

const (
	metricsMessageLeg       metricsMessage = "logging.request.leg"
	metricsMessageStarted   metricsMessage = "adapter.request.started"
	metricsMessageStreamed  metricsMessage = "adapter.request.stream_opened"
	metricsMessageIO        metricsMessage = "adapter.request.io"
	metricsMessageCompleted metricsMessage = "adapter.request.completed"
	metricsMessageFailed    metricsMessage = "adapter.request.failed"
	metricsMessageCancelled metricsMessage = "adapter.request.cancelled"
)

type metricsRequestStatus string

const (
	metricsRequestStatusCompleted metricsRequestStatus = "completed"
	metricsRequestStatusFailed    metricsRequestStatus = "failed"
	metricsRequestStatusCancelled metricsRequestStatus = "cancelled"
)

// MetricsHistoryInput identifies the Clyde daemon log and requested window.
type MetricsHistoryInput struct {
	Since   time.Time
	Now     time.Time
	LogPath string
	Pricing adapterruntime.PricingTable
}

// MetricsHistoryFromConfig reads the configured Clyde daemon log history.
func MetricsHistoryFromConfig(since time.Duration, now time.Time) MetricsHistoryReport {
	windowStart := now.Add(-since)
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return MetricsHistoryReport{
			Window:                 MetricsWindow{Since: windowStart, Until: now},
			Coverage:               MetricsCoverage{Complete: false},
			Metrics:                emptyMetricsValues(),
			TimeBreakdown:          MetricsTimeBreakdown{Total: emptyMetricsDuration(), Stages: []MetricsStageDuration{}},
			UnattributedDurationMS: nil,
			Warnings:               []string{"unable to load Clyde logging configuration"},
			historyCoversStart:     false,
			liveCoversEnd:          false,
			generationStable:       false,
			invalidHistory:         true,
			sawCanonicalRecord:     false,
		}
	}
	return BuildMetricsHistory(MetricsHistoryInput{
		Since:   windowStart,
		Now:     now,
		LogPath: slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon),
		Pricing: adapterruntime.NewPricingTable(cfg.Adapter.ModelPricing()),
	})
}

// MetricsHistoryReport is the typed historical daemon metrics payload.
type MetricsHistoryReport struct {
	Window                 MetricsWindow        `json:"window"`
	Coverage               MetricsCoverage      `json:"coverage"`
	Metrics                MetricsValues        `json:"metrics"`
	TimeBreakdown          MetricsTimeBreakdown `json:"time_breakdown"`
	UnattributedDurationMS *int64               `json:"unattributed_duration_ms"`
	// Restarts counts daemon generations that began inside the window. A
	// window spanning a restart sums across the discontinuity, so this is what
	// tells the reader how many resets the summed totals absorb.
	Restarts           int      `json:"restarts"`
	Warnings           []string `json:"warnings"`
	historyCoversStart bool     `json:"-"`
	liveCoversEnd      bool     `json:"-"`
	generationStable   bool     `json:"-"`
	invalidHistory     bool     `json:"-"`
	sawCanonicalRecord bool     `json:"-"`
}

// MetricsWindow describes the included interval.
type MetricsWindow struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// MetricsCoverage reports whether retained Clyde history fully brackets the window.
type MetricsCoverage struct {
	Complete bool `json:"complete"`
}

// MetricsCounter reports a monotonic value at the end of a window.
type MetricsCounter struct {
	Current       *int64   `json:"current"`
	Delta         *int64   `json:"delta"`
	RatePerSecond *float64 `json:"rate_per_second"`
}

// MetricsGauge reports an observed non-monotonic value.
type MetricsGauge struct {
	Current *int64   `json:"current"`
	Min     *int64   `json:"min"`
	Mean    *float64 `json:"mean"`
	Max     *int64   `json:"max"`
}

// MetricsDuration reports latency samples in milliseconds.
type MetricsDuration struct {
	TotalMS *int64   `json:"total_ms"`
	Calls   *int64   `json:"calls"`
	MeanMS  *float64 `json:"mean_ms"`
	P50MS   *int64   `json:"p50_ms"`
	P95MS   *int64   `json:"p95_ms"`
	MaxMS   *int64   `json:"max_ms"`
}

// MetricsValues contains request, provider, token, byte, and activity metrics.
type MetricsValues struct {
	Requests       MetricsCounter `json:"requests"`
	Completed      MetricsCounter `json:"completed"`
	Failed         MetricsCounter `json:"failed"`
	Cancelled      MetricsCounter `json:"cancelled"`
	BytesIn        MetricsCounter `json:"bytes_in"`
	BytesOut       MetricsCounter `json:"bytes_out"`
	InputTokens    MetricsCounter `json:"input_tokens"`
	OutputTokens   MetricsCounter `json:"output_tokens"`
	CacheTokens    MetricsCounter `json:"cache_tokens"`
	CostMicrocents MetricsCounter `json:"cost_microcents"`
	Inflight       MetricsGauge   `json:"inflight"`
	Streaming      MetricsGauge   `json:"streaming"`
}

// MetricsTimeBreakdown contains total latency and exclusive lifecycle stages.
type MetricsTimeBreakdown struct {
	Total  MetricsDuration        `json:"total"`
	Stages []MetricsStageDuration `json:"stages"`
}

// MetricsStageDuration names one ranked request stage.
type MetricsStageDuration struct {
	Name string `json:"name"`
	MetricsDuration
}

type metricsLogRecord struct {
	Time                       string         `json:"time"`
	Message                    metricsMessage `json:"msg"`
	RequestID                  string         `json:"request_id"`
	ExecutionID                string         `json:"execution_id"`
	Leg                        string         `json:"leg"`
	DurationMS                 int64          `json:"duration_ms"`
	Status                     string         `json:"status"`
	Provider                   string         `json:"provider"`
	Model                      string         `json:"model"`
	BytesIn                    int64          `json:"bytes_in"`
	BytesOut                   int64          `json:"bytes_out"`
	PromptTokens               int64          `json:"prompt_tokens"`
	CompletionTokens           int64          `json:"completion_tokens"`
	CacheReadTokens            int64          `json:"cache_read_tokens"`
	CacheCreationTokens        int64          `json:"cache_creation_tokens"`
	DerivedCacheCreationTokens int64          `json:"derived_cache_creation_tokens"`
}

type metricsLogEnvelope struct {
	Time    string         `json:"time"`
	Message metricsMessage `json:"msg"`
}

type metricsRequest struct {
	started             bool
	terminal            bool
	lifecycleStarted    bool
	lifecycleTerminal   bool
	invalidLifecycle    bool
	status              metricsRequestStatus
	totalMS             int64
	bytesIn             int64
	bytesOut            int64
	promptTokens        int64
	outputTokens        int64
	cacheTokens         int64
	cacheReadTokens     int64
	cacheCreationTokens int64
	model               string
	ioSeen              bool
	startedAt           time.Time
	streamedAt          time.Time
	terminalAt          time.Time
	legDurations        map[string]int64
}

// BuildMetricsHistory reads only the canonical daemon JSONL file and its rotations.
func BuildMetricsHistory(input MetricsHistoryInput) MetricsHistoryReport {
	report := MetricsHistoryReport{
		Window:                 MetricsWindow{Since: input.Since, Until: input.Now},
		Coverage:               MetricsCoverage{Complete: false},
		Metrics:                emptyMetricsValues(),
		TimeBreakdown:          MetricsTimeBreakdown{Total: emptyMetricsDuration(), Stages: []MetricsStageDuration{}},
		UnattributedDurationMS: nil,
		Warnings:               []string{},
		historyCoversStart:     false,
		liveCoversEnd:          false,
		generationStable:       true,
		invalidHistory:         false,
		sawCanonicalRecord:     false,
	}
	if input.Now.Before(input.Since) || input.LogPath == "" {
		report.Coverage.Complete = false
		report.Warnings = append(report.Warnings, "invalid metrics window or daemon log path")
		return report
	}

	requests := make(map[string]*metricsRequest)
	for _, path := range daemonLogHistoryPaths(input.LogPath, input.Since) {
		readMetricsHistoryFile(path, input, requests, &report)
	}
	if !report.sawCanonicalRecord {
		addMetricsWarning(&report, "no retained daemon history in the requested window")
	} else if !report.historyCoversStart {
		addMetricsWarning(&report, "retained daemon history starts after the requested window")
	}
	aggregateMetricsRequests(&report, requests, input.Pricing)
	updateMetricsCoverage(&report)
	return report
}

func daemonLogHistoryPaths(logPath string, since time.Time) []string {
	entries, err := os.ReadDir(filepath.Dir(logPath))
	if err != nil {
		return []string{logPath}
	}
	base := filepath.Base(logPath)
	rotationNamePattern := daemonRotationPattern(base)
	activePath := ""
	rotations := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(filepath.Dir(logPath), name)
		if name == base {
			activePath = path
		}
		if rotationNamePattern.MatchString(name) {
			rotations = append(rotations, path)
		}
	}
	slices.Sort(rotations)
	paths := rotationsInsideWindow(rotations, since, base)
	if activePath != "" {
		paths = append(paths, activePath)
	}
	if activePath == "" {
		paths = append(paths, logPath)
	}
	return paths
}

func daemonRotationPattern(activeBase string) *regexp.Regexp {
	extension := filepath.Ext(activeBase)
	stem := strings.TrimSuffix(activeBase, extension)
	pattern := `^` + regexp.QuoteMeta(stem) + `-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{3}` +
		regexp.QuoteMeta(extension) + `(?:\.gz)?$`
	return regexp.MustCompile(pattern)
}

func rotationsInsideWindow(rotations []string, since time.Time, activeBase string) []string {
	extension := filepath.Ext(activeBase)
	rotationPrefix := strings.TrimSuffix(activeBase, extension) + "-"
	firstInside := len(rotations)
	for i, path := range rotations {
		name := filepath.Base(path)
		timestamp := strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), extension)
		timestamp = strings.TrimPrefix(timestamp, rotationPrefix)
		rotatedAt, err := time.ParseInLocation("2006-01-02T15-04-05.000", timestamp, since.Location())
		if err == nil && !rotatedAt.Before(since) {
			firstInside = i
			break
		}
	}
	start := firstInside
	if start > 0 {
		start--
	}
	return append([]string(nil), rotations[start:]...)
}

func readMetricsHistoryFile(path string, input MetricsHistoryInput, requests map[string]*metricsRequest, report *MetricsHistoryReport) {
	reader, closeReader, err := openMetricsHistoryFile(path)
	if err != nil {
		report.invalidHistory = true
		addMetricsWarning(report, "unreadable daemon log rotation: "+filepath.Base(path))
		return
	}
	defer func() { _ = closeReader() }()

	scanner := bufio.NewScanner(reader)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 2*1024*1024)
	for scanner.Scan() {
		var envelope metricsLogEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			report.invalidHistory = true
			addMetricsWarning(report, "malformed daemon log record in "+filepath.Base(path))
			continue
		}
		recordedAt, err := time.Parse(time.RFC3339Nano, envelope.Time)
		if err != nil {
			if isMetricsMessage(envelope.Message) {
				report.invalidHistory = true
				addMetricsWarning(report, "malformed relevant timestamp in "+filepath.Base(path))
			}
			continue
		}
		if recordedAt.After(input.Now) {
			continue
		}
		report.sawCanonicalRecord = true
		if !recordedAt.After(input.Since) {
			report.historyCoversStart = true
		}
		if envelope.Message == "daemon.worker.ready" && !recordedAt.Before(input.Since) {
			report.generationStable = false
			addMetricsWarning(report, "daemon provider counter generation restarted inside the requested window")
		}
		if !isMetricsMessage(envelope.Message) {
			continue
		}
		var record metricsLogRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			report.invalidHistory = true
			addMetricsWarning(report, "malformed daemon metrics record in "+filepath.Base(path))
			continue
		}
		if record.RequestID == "" {
			continue
		}
		addMetricsHistoryRecord(record, recordedAt, requests, report)
	}
	if err := scanner.Err(); err != nil {
		report.invalidHistory = true
		addMetricsWarning(report, "unreadable daemon log rotation: "+filepath.Base(path))
	}
}

func isMetricsMessage(message metricsMessage) bool {
	switch message {
	case metricsMessageLeg, metricsMessageStarted, metricsMessageStreamed, metricsMessageIO,
		metricsMessageCompleted, metricsMessageFailed, metricsMessageCancelled:
		return true
	default:
		return false
	}
}

func addMetricsWarning(report *MetricsHistoryReport, warning string) {
	if !slices.Contains(report.Warnings, warning) {
		report.Warnings = append(report.Warnings, warning)
	}
}

func openMetricsHistoryFile(path string) (io.Reader, func() error, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open daemon metrics history: %w", err)
	}
	if !strings.HasSuffix(path, ".gz") {
		return file, file.Close, nil
	}
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		slog.Warn("daemon.metrics_history.compressed_open_failed", "concern", "process.daemon.lifecycle", "path", path, "err", err)
		return nil, nil, fmt.Errorf("open compressed daemon metrics history: %w", err)
	}
	return gzipReader, func() error {
		gzipErr := gzipReader.Close()
		fileErr := file.Close()
		if gzipErr != nil {
			return fmt.Errorf("close compressed daemon metrics history: %w", gzipErr)
		}
		if fileErr != nil {
			return fmt.Errorf("close daemon metrics history: %w", fileErr)
		}
		return nil
	}, nil
}

func addMetricsHistoryRecord(
	record metricsLogRecord,
	recordedAt time.Time,
	requests map[string]*metricsRequest,
	report *MetricsHistoryReport,
) {
	executionKey := strings.TrimSpace(record.ExecutionID)
	if executionKey == "" {
		executionKey = record.RequestID
	}
	request := requests[executionKey]
	if request == nil {
		request = &metricsRequest{
			started: false, terminal: false, lifecycleStarted: false, lifecycleTerminal: false,
			invalidLifecycle: false, status: "", totalMS: 0, bytesIn: 0, bytesOut: 0,
			promptTokens: 0, outputTokens: 0, cacheTokens: 0, cacheReadTokens: 0,
			cacheCreationTokens: 0, model: "", ioSeen: false, startedAt: time.Time{},
			streamedAt: time.Time{}, terminalAt: time.Time{}, legDurations: make(map[string]int64),
		}
		requests[executionKey] = request
	}
	if record.Message == metricsMessageLeg {
		request.legDurations[record.Leg] = max(request.legDurations[record.Leg], record.DurationMS)
		request.bytesIn += record.BytesIn
		request.bytesOut += record.BytesOut
		if record.Leg == "adapter_ingress" {
			request.started = true
		}
		if record.Leg == "request_error" || record.Status == "error" {
			request.terminal = true
			request.status = metricsRequestStatusFailed
			request.totalMS = max(request.totalMS, record.DurationMS)
		}
	}
	applyMetricsCanonicalRecord(record, recordedAt, request, report)
}

func applyMetricsCanonicalRecord(
	record metricsLogRecord,
	recordedAt time.Time,
	request *metricsRequest,
	report *MetricsHistoryReport,
) {
	switch record.Message {
	case metricsMessageLeg:
		return
	case metricsMessageStarted:
		if request.lifecycleStarted {
			invalidateMetricsLifecycle(report, request, "request has duplicate start records")
			return
		}
		request.lifecycleStarted = true
		request.started = true
		request.startedAt = recordedAt
		validateMetricsStartedTimestamp(report, request)
	case metricsMessageStreamed:
		if !request.streamedAt.IsZero() {
			invalidateMetricsLifecycle(report, request, "request has duplicate stream records")
		}
		if !request.lifecycleStarted {
			invalidateMetricsLifecycle(report, request, "request stream record precedes start")
		}
		if request.lifecycleTerminal {
			invalidateMetricsLifecycle(report, request, "request stream record follows terminal")
		}
		if request.streamedAt.IsZero() {
			request.streamedAt = recordedAt
		}
		validateMetricsStreamedTimestamp(report, request)
	case metricsMessageIO:
		if !request.ioSeen {
			request.bytesIn = record.BytesIn
			request.bytesOut = record.BytesOut
			request.ioSeen = true
		}
	case metricsMessageCompleted, metricsMessageFailed, metricsMessageCancelled:
		if request.lifecycleTerminal {
			invalidateMetricsLifecycle(report, request, "request has duplicate terminal records")
			return
		}
		if !request.lifecycleStarted {
			invalidateMetricsLifecycle(report, request, "request terminal record precedes start")
		}
		request.lifecycleTerminal = true
		request.terminal = true
		request.terminalAt = recordedAt
		validateMetricsTerminalTimestamp(report, request)
		request.totalMS = max(request.totalMS, record.DurationMS)
		request.promptTokens = max(request.promptTokens, record.PromptTokens)
		request.outputTokens = max(request.outputTokens, record.CompletionTokens)
		request.cacheTokens = max(request.cacheTokens, record.CacheReadTokens+record.CacheCreationTokens+record.DerivedCacheCreationTokens)
		request.cacheReadTokens = max(request.cacheReadTokens, record.CacheReadTokens)
		request.cacheCreationTokens = max(request.cacheCreationTokens, record.CacheCreationTokens+record.DerivedCacheCreationTokens)
		request.model = record.Model
		request.bytesIn += record.BytesIn
		request.bytesOut += record.BytesOut
		switch record.Message {
		case metricsMessageCompleted:
			request.status = metricsRequestStatusCompleted
		case metricsMessageFailed:
			request.status = metricsRequestStatusFailed
		case metricsMessageCancelled:
			request.status = metricsRequestStatusCancelled
		case metricsMessageLeg, metricsMessageStarted, metricsMessageStreamed, metricsMessageIO:
			return
		}
	}
}

func validateMetricsStartedTimestamp(report *MetricsHistoryReport, request *metricsRequest) {
	if !request.streamedAt.IsZero() && request.streamedAt.Before(request.startedAt) {
		invalidateMetricsLifecycle(report, request, "request stream timestamp precedes start timestamp")
	}
	if !request.terminalAt.IsZero() && request.startedAt.After(request.terminalAt) {
		invalidateMetricsLifecycle(report, request, "request start timestamp follows terminal timestamp")
	}
}

func validateMetricsStreamedTimestamp(report *MetricsHistoryReport, request *metricsRequest) {
	if !request.startedAt.IsZero() && request.streamedAt.Before(request.startedAt) {
		invalidateMetricsLifecycle(report, request, "request stream timestamp precedes start timestamp")
	}
	if !request.terminalAt.IsZero() && request.terminalAt.Before(request.streamedAt) {
		invalidateMetricsLifecycle(report, request, "request terminal timestamp precedes stream timestamp")
	}
}

func validateMetricsTerminalTimestamp(report *MetricsHistoryReport, request *metricsRequest) {
	if !request.startedAt.IsZero() && request.startedAt.After(request.terminalAt) {
		invalidateMetricsLifecycle(report, request, "request start timestamp follows terminal timestamp")
	}
	if !request.streamedAt.IsZero() && request.terminalAt.Before(request.streamedAt) {
		invalidateMetricsLifecycle(report, request, "request terminal timestamp precedes stream timestamp")
	}
}

func invalidateMetricsLifecycle(report *MetricsHistoryReport, request *metricsRequest, warning string) {
	request.invalidLifecycle = true
	report.invalidHistory = true
	addMetricsWarning(report, warning)
}

type metricsCounterDeltas struct {
	requests     int64
	completed    int64
	failed       int64
	cancelled    int64
	bytesIn      int64
	bytesOut     int64
	inputTokens  int64
	outputTokens int64
	cacheTokens  int64
	cost         int64
}

func aggregateMetricsRequests(report *MetricsHistoryReport, requests map[string]*metricsRequest, pricing adapterruntime.PricingTable) {
	durationSeconds := report.Window.Until.Sub(report.Window.Since).Seconds()
	totalSamples := make([]int64, 0, len(requests))
	stageSamples := make(map[string][]int64)
	unknownModels := make(map[string]bool)
	deltas := metricsCounterDeltas{
		requests: 0, completed: 0, failed: 0, cancelled: 0, bytesIn: 0,
		bytesOut: 0, inputTokens: 0, outputTokens: 0, cacheTokens: 0, cost: 0,
	}
	for _, request := range requests {
		if request.invalidLifecycle {
			continue
		}
		if !request.terminal {
			continue
		}
		if request.terminalAt.Before(report.Window.Since) || request.terminalAt.After(report.Window.Until) {
			continue
		}
		if !request.started {
			report.invalidHistory = true
			addMetricsWarning(report, "request terminal record has no retained start")
		}
		deltas.requests++
		deltas.bytesIn += request.bytesIn
		deltas.bytesOut += request.bytesOut
		deltas.inputTokens += request.promptTokens
		deltas.outputTokens += request.outputTokens
		deltas.cacheTokens += request.cacheTokens
		addRequestStatusDelta(&deltas, request.status)
		cost, unknownModel := priceMetricsRequest(request, pricing)
		deltas.cost += cost
		if unknownModel != "" {
			unknownModels[unknownModel] = true
		}
		addRequestDuration(report, request, &totalSamples, stageSamples)
	}
	if report.sawCanonicalRecord {
		setMetricCounter(&report.Metrics.Requests, deltas.requests, durationSeconds)
		setMetricCounter(&report.Metrics.Completed, deltas.completed, durationSeconds)
		setMetricCounter(&report.Metrics.Failed, deltas.failed, durationSeconds)
		setMetricCounter(&report.Metrics.Cancelled, deltas.cancelled, durationSeconds)
		setMetricCounter(&report.Metrics.BytesIn, deltas.bytesIn, durationSeconds)
		setMetricCounter(&report.Metrics.BytesOut, deltas.bytesOut, durationSeconds)
		setMetricCounter(&report.Metrics.InputTokens, deltas.inputTokens, durationSeconds)
		setMetricCounter(&report.Metrics.OutputTokens, deltas.outputTokens, durationSeconds)
		setMetricCounter(&report.Metrics.CacheTokens, deltas.cacheTokens, durationSeconds)
		if len(unknownModels) == 0 {
			setMetricCounter(&report.Metrics.CostMicrocents, deltas.cost, durationSeconds)
		}
	}
	unknownModelNames := make([]string, 0, len(unknownModels))
	for model := range unknownModels {
		unknownModelNames = append(unknownModelNames, model)
	}
	slices.Sort(unknownModelNames)
	for _, model := range unknownModelNames {
		addMetricsWarning(report, "pricing unavailable for model "+model)
	}
	report.TimeBreakdown.Total = durationFromSamples(totalSamples)
	report.TimeBreakdown.Stages = rankedStageDurations(stageSamples)
	inflightSamples, streamingSamples := lifecycleGaugeSamples(requests, report.Window)
	report.Metrics.Inflight = gaugeFromSamples(inflightSamples)
	report.Metrics.Streaming = gaugeFromSamples(streamingSamples)
	if len(totalSamples) > 0 && report.UnattributedDurationMS == nil {
		report.UnattributedDurationMS = new(int64(0))
	}
}

func addRequestStatusDelta(deltas *metricsCounterDeltas, status metricsRequestStatus) {
	switch status {
	case metricsRequestStatusCompleted:
		deltas.completed++
	case metricsRequestStatusFailed:
		deltas.failed++
	case metricsRequestStatusCancelled:
		deltas.cancelled++
	default:
		deltas.completed++
	}
}

func priceMetricsRequest(request *metricsRequest, pricing adapterruntime.PricingTable) (int64, string) {
	if request.promptTokens == 0 && request.outputTokens == 0 && request.cacheTokens == 0 {
		return 0, ""
	}
	cost := adapterruntime.EstimateCost(adapterruntime.CostInputs{
		ModelID: request.model, TTL: "", InputTokens: int(request.promptTokens), OutputTokens: int(request.outputTokens),
		CacheCreationTokens: int(request.cacheCreationTokens), CacheReadTokens: int(request.cacheReadTokens),
	}, pricing)
	if cost.RatesKnown {
		return cost.TotalMicrocents, ""
	}
	model := strings.TrimSpace(request.model)
	if model == "" {
		model = "<missing>"
	}
	return 0, model
}

func addRequestDuration(report *MetricsHistoryReport, request *metricsRequest, totalSamples *[]int64, stageSamples map[string][]int64) {
	if request.startedAt.IsZero() || request.terminalAt.Before(request.startedAt) {
		return
	}
	totalMS := request.terminalAt.Sub(request.startedAt).Milliseconds()
	*totalSamples = append(*totalSamples, totalMS)
	requestStages := exclusiveStageSamples(request)
	var stageTotal int64
	for stage, duration := range requestStages {
		stageTotal += duration
		stageSamples[stage] = append(stageSamples[stage], duration)
	}
	if totalMS <= stageTotal {
		return
	}
	if report.UnattributedDurationMS == nil {
		report.UnattributedDurationMS = new(int64(0))
	}
	*report.UnattributedDurationMS += totalMS - stageTotal
}

func exclusiveStageSamples(request *metricsRequest) map[string]int64 {
	stages := make(map[string]int64)
	if !request.startedAt.IsZero() && !request.streamedAt.IsZero() && !request.terminalAt.IsZero() {
		firstResponse := request.streamedAt.Sub(request.startedAt).Milliseconds()
		streaming := request.terminalAt.Sub(request.streamedAt).Milliseconds()
		if firstResponse >= 0 {
			stages["provider_to_first_response"] = firstResponse
		}
		if streaming >= 0 {
			stages["response_streaming"] = streaming
		}
		return stages
	}
	if request.streamedAt.IsZero() && !request.startedAt.IsZero() && !request.terminalAt.IsZero() {
		total := request.terminalAt.Sub(request.startedAt).Milliseconds()
		if total >= 0 {
			stages["provider_total"] = total
		}
		return stages
	}
	return stages
}

func setMetricCounter(counter *MetricsCounter, delta int64, seconds float64) {
	counter.Delta = new(delta)
	if seconds > 0 {
		counter.RatePerSecond = new(float64(delta) / seconds)
	}
}

func rankedStageDurations(samples map[string][]int64) []MetricsStageDuration {
	stages := make([]MetricsStageDuration, 0, len(samples))
	for name, values := range samples {
		stages = append(stages, MetricsStageDuration{Name: name, MetricsDuration: durationFromSamples(values)})
	}
	slices.SortFunc(stages, func(a, b MetricsStageDuration) int {
		if *a.TotalMS != *b.TotalMS {
			return int(*b.TotalMS - *a.TotalMS)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return stages
}

func durationFromSamples(samples []int64) MetricsDuration {
	if len(samples) == 0 {
		return emptyMetricsDuration()
	}
	slices.Sort(samples)
	var total int64
	for _, sample := range samples {
		total += sample
	}
	return MetricsDuration{
		TotalMS: new(total),
		Calls:   new(int64(len(samples))),
		MeanMS:  new(float64(total) / float64(len(samples))),
		P50MS:   new(percentile(samples, 0.50)),
		P95MS:   new(percentile(samples, 0.95)),
		MaxMS:   new(samples[len(samples)-1]),
	}
}

func percentile(samples []int64, fraction float64) int64 {
	if len(samples) == 0 {
		return 0
	}
	index := int(float64(len(samples))*fraction+0.999999999) - 1
	index = max(0, min(index, len(samples)-1))
	return samples[index]
}

type metricsGaugeEvent struct {
	at             time.Time
	inflightDelta  int64
	streamingDelta int64
}

func lifecycleGaugeSamples(requests map[string]*metricsRequest, window MetricsWindow) ([]int64, []int64) {
	events := make([]metricsGaugeEvent, 0, len(requests)*3)
	var inflight int64
	var streaming int64
	hasIntersection := false
	for _, request := range requests {
		if !request.startedAt.IsZero() && !request.startedAt.After(window.Since) &&
			(request.terminalAt.IsZero() || request.terminalAt.After(window.Since)) {
			inflight++
			hasIntersection = true
		}
		if !request.streamedAt.IsZero() && !request.streamedAt.After(window.Since) &&
			(request.terminalAt.IsZero() || request.terminalAt.After(window.Since)) {
			streaming++
			hasIntersection = true
		}
		if request.startedAt.After(window.Since) && !request.startedAt.After(window.Until) {
			events = append(events, metricsGaugeEvent{at: request.startedAt, inflightDelta: 1, streamingDelta: 0})
			hasIntersection = true
		}
		if request.streamedAt.After(window.Since) && !request.streamedAt.After(window.Until) {
			events = append(events, metricsGaugeEvent{at: request.streamedAt, inflightDelta: 0, streamingDelta: 1})
			hasIntersection = true
		}
		if request.terminalAt.After(window.Since) && !request.terminalAt.After(window.Until) {
			events = append(events, metricsGaugeEvent{at: request.terminalAt, inflightDelta: -1, streamingDelta: 0})
			if !request.streamedAt.IsZero() {
				events[len(events)-1].streamingDelta = -1
			}
			hasIntersection = true
		}
	}
	if !hasIntersection {
		return nil, nil
	}
	slices.SortFunc(events, func(a, b metricsGaugeEvent) int { return a.at.Compare(b.at) })
	inflightSamples := []int64{inflight}
	streamingSamples := []int64{streaming}
	for _, event := range events {
		inflight = max(0, inflight+event.inflightDelta)
		streaming = max(0, streaming+event.streamingDelta)
		inflightSamples = append(inflightSamples, inflight)
		streamingSamples = append(streamingSamples, streaming)
	}
	return inflightSamples, streamingSamples
}

func gaugeFromSamples(samples []int64) MetricsGauge {
	if len(samples) == 0 {
		return MetricsGauge{Current: nil, Min: nil, Mean: nil, Max: nil}
	}
	minimum := samples[0]
	maxValue := samples[0]
	var total int64
	for _, sample := range samples {
		minimum = min(minimum, sample)
		maxValue = max(maxValue, sample)
		total += sample
	}
	return MetricsGauge{Current: nil, Min: new(minimum), Mean: new(float64(total) / float64(len(samples))), Max: new(maxValue)}
}

func emptyMetricsValues() MetricsValues {
	return MetricsValues{
		Requests: emptyMetricsCounter(), Completed: emptyMetricsCounter(), Failed: emptyMetricsCounter(), Cancelled: emptyMetricsCounter(),
		BytesIn: emptyMetricsCounter(), BytesOut: emptyMetricsCounter(), InputTokens: emptyMetricsCounter(), OutputTokens: emptyMetricsCounter(),
		CacheTokens: emptyMetricsCounter(), CostMicrocents: emptyMetricsCounter(),
		Inflight:  MetricsGauge{Current: nil, Min: nil, Mean: nil, Max: nil},
		Streaming: MetricsGauge{Current: nil, Min: nil, Mean: nil, Max: nil},
	}
}

func emptyMetricsCounter() MetricsCounter {
	return MetricsCounter{Current: nil, Delta: nil, RatePerSecond: nil}
}

// ApplyCurrentProviderSnapshot overlays the live daemon provider API and closes the window.
func ApplyCurrentProviderSnapshot(report *MetricsHistoryReport, providers []ProviderStats, loadedAt time.Time) {
	if report == nil {
		return
	}
	var requests, inputTokens, outputTokens, cacheTokens, cost, inflight, streaming int64
	for _, provider := range providers {
		requests += int64(provider.Requests)
		inputTokens += provider.InputTokens
		outputTokens += provider.OutputTokens
		cacheTokens += provider.CacheReadTokens + provider.CacheCreationTokens + provider.DerivedCacheCreationTokens
		cost += provider.EstimatedCostMicrocents
		inflight += int64(provider.Inflight)
		streaming += int64(provider.Streaming)
	}
	report.Metrics.Requests.Current = &requests
	report.Metrics.InputTokens.Current = &inputTokens
	report.Metrics.OutputTokens.Current = &outputTokens
	report.Metrics.CacheTokens.Current = &cacheTokens
	report.Metrics.CostMicrocents.Current = &cost
	report.Metrics.Inflight.Current = &inflight
	report.Metrics.Streaming.Current = &streaming
	report.liveCoversEnd = true
	if !loadedAt.Before(report.Window.Since) {
		report.generationStable = false
		addMetricsWarning(report, "provider counter generation began inside the requested window")
		invalidateGenerationSensitiveCounters(&report.Metrics)
	}
	updateMetricsCoverage(report)
}

func emptyMetricsDuration() MetricsDuration {
	return MetricsDuration{TotalMS: nil, Calls: nil, MeanMS: nil, P50MS: nil, P95MS: nil, MaxMS: nil}
}

func invalidateGenerationSensitiveCounters(metrics *MetricsValues) {
	counters := []*MetricsCounter{
		&metrics.Requests,
		&metrics.InputTokens,
		&metrics.OutputTokens,
		&metrics.CacheTokens,
		&metrics.CostMicrocents,
	}
	for _, counter := range counters {
		counter.Delta = nil
		counter.RatePerSecond = nil
	}
}

func updateMetricsCoverage(report *MetricsHistoryReport) {
	if !report.generationStable {
		invalidateGenerationSensitiveCounters(&report.Metrics)
	}
	report.Coverage.Complete = report.historyCoversStart && report.liveCoversEnd &&
		report.generationStable && !report.invalidHistory
}
