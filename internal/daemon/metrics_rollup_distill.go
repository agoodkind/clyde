package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

// metricsRollupState retains the known log tail and unfinished requests between passes.
type metricsRollupState struct {
	checkpoint      metricsRollupCheckpoint
	source          *os.File
	requests        map[string]*metricsRequest
	nextExpiry      time.Time
	retentionLoaded bool
}

// metricsRollupDistillInput names one distill pass using worker-owned state.
type metricsRollupDistillInput struct {
	LogPath    string
	RollupPath string
	Now        time.Time
	Pricing    adapterruntime.PricingTable
	State      *metricsRollupState
}

// metricsRollupDistillResult reports one pass's ordinary reads and writes.
type metricsRollupDistillResult struct {
	Written      int
	Pruned       int
	BytesRead    int64
	LastRecordAt time.Time
}

// distillMetricsRollup processes complete new records with the history parser.
func distillMetricsRollup(ctx context.Context, input metricsRollupDistillInput) (metricsRollupDistillResult, error) {
	var result metricsRollupDistillResult
	if err := ctx.Err(); err != nil {
		return result, rollupError("daemon.metrics_rollup.pass_canceled", input.LogPath, err, "distill metrics rollup")
	}
	state := input.State
	if err := state.openSource(input.LogPath, input.Now); err != nil {
		return result, err
	}
	if state.requests == nil {
		state.requests = make(map[string]*metricsRequest)
	}
	var err error
	result.BytesRead, err = state.readTail(ctx, input)
	if err != nil {
		return result, err
	}
	result.LastRecordAt, _ = parseRollupTime(state.checkpoint.LastRecordAt)
	records := make([]metricsRollupRecord, 0, len(state.requests))
	for requestID, request := range state.requests {
		if !distillableRequest(request, input.Now) {
			continue
		}
		records = append(records, requestToRollupRecord(requestID, request))
		if request.terminalAt.After(result.LastRecordAt) {
			result.LastRecordAt = request.terminalAt
		}
	}
	lock, err := acquireRollupWriteLock(input.RollupPath)
	if err != nil {
		return result, err
	}
	defer func() { _ = lock.Unlock() }()
	if err := state.loadRetention(input.RollupPath); err != nil {
		return result, err
	}
	if err := appendMetricsRollupRecords(input.RollupPath, sortedRollupRecords(records)); err != nil {
		return result, err
	}
	for _, record := range records {
		delete(state.requests, record.RequestID)
		at, _ := parseRollupTime(record.TerminalAt)
		state.rememberExpiry(at)
	}
	result.Written = len(records)
	state.checkpoint.LastRecordAt = formatRollupTime(result.LastRecordAt)
	if !state.nextExpiry.IsZero() && input.Now.After(state.nextExpiry) {
		result.Pruned, err = pruneMetricsRollup(input.RollupPath, input.Now.Add(-metricsRollupRetention))
		if err != nil {
			return result, err
		}
		state.retentionLoaded = false
		if err := state.loadRetention(input.RollupPath); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *metricsRollupState) rememberExpiry(at time.Time) {
	expiry := at.Add(metricsRollupRetention)
	if s.nextExpiry.IsZero() || expiry.Before(s.nextExpiry) {
		s.nextExpiry = expiry
	}
}

func (s *metricsRollupState) loadRetention(path string) error {
	if s.retentionLoaded {
		return nil
	}
	// An empty window reuses the store reader without retaining request aggregates.
	loaded, err := loadMetricsRollup(path, []MetricsWindow{{Since: time.Time{}, Until: time.Time{}}})
	if err != nil {
		return err
	}
	s.nextExpiry = time.Time{}
	if !loaded[0].EarliestSeen.IsZero() {
		s.rememberExpiry(loaded[0].EarliestSeen)
	}
	s.retentionLoaded = true
	return nil
}

func (s *metricsRollupState) closeSource() error {
	if s.source == nil {
		return nil
	}
	err := s.source.Close()
	s.source = nil
	if err != nil {
		return rollupError("daemon.metrics_rollup.source_close_failed", s.checkpoint.Source.Path, err, "close metrics log")
	}
	return nil
}

func rollupSourcePosition(path string, info os.FileInfo) metricsRollupSourcePosition {
	stat, _ := info.Sys().(*syscall.Stat_t)
	return metricsRollupSourcePosition{Path: path, Device: strconv.FormatInt(int64(stat.Dev), 10), Inode: stat.Ino, Offset: info.Size()}
}

func (s *metricsRollupState) openSource(path string, now time.Time) error {
	if s.source != nil {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return rollupError("daemon.metrics_rollup.source_open_failed", path, err, "open metrics log")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return rollupError("daemon.metrics_rollup.source_stat_failed", path, err, "stat metrics log")
	}
	position := rollupSourcePosition(path, info)
	saved := s.checkpoint.Source
	if saved.Path == path && saved.Device == position.Device && saved.Inode == position.Inode &&
		saved.Offset >= 0 && saved.Offset <= position.Offset {
		position.Offset = saved.Offset
	} else {
		s.checkpoint.CoverageSince = formatRollupTime(now)
	}
	s.checkpoint.Source = position
	s.source = file
	return nil
}

func (s *metricsRollupState) readTail(ctx context.Context, input metricsRollupDistillInput) (int64, error) {
	info, err := s.source.Stat()
	if err != nil {
		return 0, rollupError("daemon.metrics_rollup.source_stat_failed", input.LogPath, err, "stat metrics log")
	}
	report := newRollupReport(MetricsWindow{Since: input.Now.Add(-metricsRollupRetention), Until: input.Now})
	// Lines appended after pass start must remain available for the next pass.
	replay := MetricsHistoryInput{Since: report.Window.Since, Now: time.Time{}, LogPath: input.LogPath, Pricing: input.Pricing}
	offset, count, err := readMetricsHistoryTail(ctx, s.source, s.checkpoint.Source.Offset, info.Size(), replay, s.requests, &report)
	s.checkpoint.Source.Offset = offset
	if err != nil {
		return count, err
	}
	active, err := os.Stat(input.LogPath)
	if err != nil {
		return count, rollupError("daemon.metrics_rollup.source_stat_failed", input.LogPath, err, "stat active metrics log")
	}
	if os.SameFile(info, active) {
		return count, nil
	}
	// The held descriptor still reads the old tail after rename and compression.
	if err := s.closeSource(); err != nil {
		return count, err
	}
	s.checkpoint.Source = rollupSourcePosition(input.LogPath, active)
	s.checkpoint.Source.Offset = 0
	if err := s.openSource(input.LogPath, input.Now); err != nil {
		return count, err
	}
	active, err = s.source.Stat()
	if err != nil {
		return count, rollupError("daemon.metrics_rollup.source_stat_failed", input.LogPath, err, "stat new metrics log")
	}
	offset, newCount, err := readMetricsHistoryTail(ctx, s.source, 0, active.Size(), replay, s.requests, &report)
	s.checkpoint.Source.Offset = offset
	return count + newCount, err
}

// readMetricsHistoryTail bounds a pass to the observed file size and each line
// to the same limit as raw replay. An unterminated final line stays pending.
func readMetricsHistoryTail(ctx context.Context, source io.ReadSeeker, offset int64, end int64, input MetricsHistoryInput, requests map[string]*metricsRequest, report *MetricsHistoryReport) (int64, int64, error) {
	if _, err := source.Seek(offset, io.SeekStart); err != nil {
		return offset, 0, rollupError("daemon.metrics_rollup.source_seek_failed", input.LogPath, err, "seek metrics log")
	}
	reader := &metricsTailReader{source: io.LimitReader(source, end-offset), bytes: 0}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	scanner.Split(func(data []byte, _ bool) (int, []byte, error) {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			return newline + 1, data[:newline+1], nil
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return offset, reader.bytes, rollupError("daemon.metrics_rollup.pass_canceled", input.LogPath, err, "read metrics log")
		}
		line := scanner.Bytes()
		readMetricsHistoryRecord(input.LogPath, line, input, requests, report)
		offset += int64(len(line))
	}
	if err := scanner.Err(); err != nil {
		return offset, reader.bytes, rollupError("daemon.metrics_rollup.source_read_failed", input.LogPath, err, "read metrics log")
	}
	return offset, reader.bytes, nil
}

type metricsTailReader struct {
	source io.Reader
	bytes  int64
}

func (r *metricsTailReader) Read(buffer []byte) (int, error) {
	n, err := r.source.Read(buffer)
	r.bytes += int64(n)
	if err == io.EOF {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read metrics tail: %w", err)
	}
	return n, nil
}

// distillableRequest excludes unfinished or invalid request lifecycles.
func distillableRequest(request *metricsRequest, now time.Time) bool {
	return request != nil && request.terminal && !request.invalidLifecycle &&
		!request.terminalAt.IsZero() && !request.terminalAt.After(now)
}
