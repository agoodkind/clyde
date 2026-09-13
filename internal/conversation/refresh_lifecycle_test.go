package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

func TestCanceledBackgroundRefreshDoesNotRecreateCache(t *testing.T) {
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	idx := testIndex(t, func(ctx context.Context, _ *Registry, _ scanCache) (scanResult, error) {
		started <- ctx
		<-release
		return scanResult{changed: true, records: []Record{{ID: "cursor:controlled", ArtifactPath: "controlled"}}}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	idx.refreshAsync(ctx, RefreshReasonPeriodic)
	scanContext := <-started
	idx.mu.Lock()
	run := idx.refreshRun
	idx.mu.Unlock()
	cancel()
	if err := os.RemoveAll(filepath.Dir(idx.cachePath)); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-run.done
	if scanContext.Err() == nil {
		t.Error("background scan lost lifecycle cancellation")
	}
	if _, err := os.Stat(idx.cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache writer recreated removed directory after cancellation: %v", err)
	}
}

func TestUnchangedRefreshDoesNotRewriteCache(t *testing.T) {
	idx := testIndex(t, scan)
	if err := idx.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	marker := time.Unix(100, 0)
	if err := os.Chtimes(idx.cachePath, marker, marker); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := idx.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	idx.debounce = 0
	idx.refreshAsync(t.Context(), RefreshReasonPeriodic)
	if err := idx.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(idx.cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(marker) {
		t.Fatal("unchanged refresh rewrote the cache")
	}
}

func TestRefreshPersistsMetadataProgressAndRemovals(t *testing.T) {
	const path = "/controlled/events.jsonl"
	parser := &recordingMultiScanParser{
		candidate: ScanCandidate{Path: path, Stamp: FileStamp{Size: 10}},
		result:    MultiConversationScanResult{Records: []Record{{ID: "copilot:one", ArtifactPath: path, Title: "first"}}, CompleteOffset: 8},
		found:     true,
	}
	idx := testIndex(t, scan, parser)
	if err := idx.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	parser.candidate.MetadataChanged = true
	parser.result.Records = []Record{{ID: "copilot:one", ArtifactPath: path, Title: "renamed", Archived: true}}
	if err := idx.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	cache, _, err := readCache(idx.cachePath)
	if err != nil || len(cache.Records) != 1 || cache.Records[0].Title != "renamed" || !cache.Records[0].Archived || cache.MultiStates[path].CompleteOffset != 8 {
		t.Fatalf("metadata was not persisted: records=%+v states=%+v err=%v", cache.Records, cache.MultiStates, err)
	}
	parser.candidate.MetadataChanged = false
	parser.candidate.Stamp.Size = 12
	parser.result.CompleteOffset = 12
	if err := idx.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	cache, _, err = readCache(idx.cachePath)
	if err != nil || cache.MultiStates[path].CompleteOffset != 12 {
		t.Fatalf("append progress was not persisted: states=%+v err=%v", cache.MultiStates, err)
	}
	reloaded := testIndex(t, scan, parser)
	reloaded.cachePath = idx.cachePath
	if err := reloaded.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(parser.scans) != 3 {
		t.Fatalf("reload reparsed unchanged records: scans=%d", len(parser.scans))
	}
	idx.registry = NewRegistry()
	if err := idx.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	cache, _, err = readCache(idx.cachePath)
	if err != nil || len(cache.Records) != 0 || len(cache.Stamps) != 0 || len(cache.MultiStates) != 0 {
		t.Fatalf("removed source remained in cache: cache=%+v err=%v", cache, err)
	}
}

func TestCachedReadsNeverStartRefresh(t *testing.T) {
	idx := testIndex(t, scan)
	for range 3 {
		if _, err := idx.List(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := idx.ListWithStamps(t.Context()); err != nil {
			t.Fatal(err)
		}
		idx.mu.Lock()
		started := idx.refreshing || !idx.lastRefresh.IsZero()
		idx.mu.Unlock()
		if started {
			t.Fatal("cached read started a refresh")
		}
	}
	if _, err := os.Stat(idx.cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive reads wrote a cache: %v", err)
	}
}

func TestIndexStartJoinsCanceledScan(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	idx := testIndex(t, func(ctx context.Context, _ *Registry, _ scanCache) (scanResult, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return scanResult{changed: true}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		idx.Start(ctx, time.Minute)
	}()
	<-started
	cancel()
	<-canceled
	select {
	case <-done:
		t.Fatal("Start returned while the canceled scan was still running")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not join the canceled scan")
	}
	if _, err := os.Stat(idx.cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled worker wrote cache: %v", err)
	}
}

func TestIndexStartWaitsUntilCompletedCacheIsDue(t *testing.T) {
	for _, records := range [][]Record{nil, {{ID: "cursor:cached", ArtifactPath: "cached"}}} {
		var installedRecord *Record
		if len(records) > 0 {
			installedRecord = &records[0]
		}
		installDiscoveries := &atomic.Int64{}
		installed := testIndex(t, scan, &cachedRefreshParser{
			discoveries: installDiscoveries,
			record:      installedRecord,
		})
		if err := installed.RefreshWithReason(t.Context(), RefreshReasonInstall); err != nil {
			t.Fatal(err)
		}
		if installDiscoveries.Load() != 1 {
			t.Fatalf("install discoveries = %d, want 1", installDiscoveries.Load())
		}
		cache, _, err := readCache(installed.cachePath)
		if err != nil {
			t.Fatal(err)
		}
		if len(cache.Records) != len(records) {
			t.Fatalf("installed records = %d, want %d", len(cache.Records), len(records))
		}
		if err := writeCache(installed.cachePath, cache.Records, cache.Stamps, cache.MultiStates, clock.Now().Add(-150*time.Millisecond)); err != nil {
			t.Fatal(err)
		}

		discoveries := &atomic.Int64{}
		restarted := testIndex(t, scan, &cachedRefreshParser{
			discoveries: discoveries,
			record:      installedRecord,
		})
		restarted.cachePath = installed.cachePath
		restarted.debounce = 0
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			restarted.Start(ctx, 250*time.Millisecond)
		}()
		time.Sleep(30 * time.Millisecond)
		if discoveries.Load() != 0 {
			t.Fatalf("discoveries before saved deadline = %d, want 0", discoveries.Load())
		}
		waitForDiscoveriesWithin(t, discoveries, 1, 150*time.Millisecond)
		cancel()
		<-done
	}
}

func TestIndexStartCacheAgeControlsFirstRefresh(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		write     func(*testing.T, string)
		immediate bool
	}{
		{name: "missing", write: func(*testing.T, string) {}, immediate: true},
		{name: "expired", write: func(t *testing.T, path string) {
			t.Helper()
			if err := writeCache(path, nil, nil, nil, time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
		}, immediate: true},
		{name: "legacy", write: func(t *testing.T, path string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"version":5,"records":[],"stamps":{}}`)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}, immediate: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			discoveries := &atomic.Int64{}
			idx := testIndex(t, scan, &cachedRefreshParser{discoveries: discoveries})
			testCase.write(t, idx.cachePath)
			idx.debounce = 0
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				idx.Start(ctx, 200*time.Millisecond)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			if testCase.immediate {
				waitForDiscoveries(t, discoveries, 1)
				return
			}
			time.Sleep(50 * time.Millisecond)
			if discoveries.Load() != 0 {
				t.Fatalf("legacy discoveries before interval = %d, want 0", discoveries.Load())
			}
			waitForDiscoveries(t, discoveries, 1)
		})
	}
}

func waitForDiscoveries(t *testing.T, discoveries *atomic.Int64, want int64) {
	t.Helper()
	waitForDiscoveriesWithin(t, discoveries, want, time.Second)
}

func waitForDiscoveriesWithin(t *testing.T, discoveries *atomic.Int64, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for discoveries.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("discoveries = %d, want %d", discoveries.Load(), want)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRefreshReasonValuesAndLifecycleFields(t *testing.T) {
	for reason, want := range map[RefreshReason]string{
		RefreshReasonInstall:    "install",
		RefreshReasonPeriodic:   "periodic",
		RefreshReasonLookupMiss: "lookup_miss",
		RefreshReasonExplicit:   "explicit",
	} {
		if reason.String() != want {
			t.Fatalf("reason = %q, want %q", reason.String(), want)
		}
	}

	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	parser := &cachedRefreshParser{discoveries: &atomic.Int64{}, record: nil}
	idx := testIndex(t, scan, parser)
	if err := idx.RefreshWithReason(t.Context(), RefreshReasonInstall); err != nil {
		t.Fatal(err)
	}

	events := decodeRefreshEvents(t, output.Bytes())
	if len(events) != 2 {
		t.Fatalf("events = %d, want start and finish: %s", len(events), output.String())
	}
	for _, event := range events {
		if event.Reason != "install" || event.Provider != "cursor" {
			t.Fatalf("attribution = reason %q provider %q", event.Reason, event.Provider)
		}
	}
	start := events[0]
	if start.Message != "conversation.index.refresh_started" || start.Outcome != "in_progress" || start.DurationMS != 0 || start.RecordCount != 0 || start.CacheChanged {
		t.Fatalf("start = %+v", start)
	}
	for _, field := range []string{"duration_ms", "outcome", "record_count", "cache_changed"} {
		if _, ok := start.Fields[field]; !ok {
			t.Fatalf("start missing %q: %+v", field, start)
		}
	}
	finish := events[1]
	if finish.Message != "conversation.index.refresh_finished" || finish.Outcome != "success" || finish.RecordCount != 0 || !finish.CacheChanged {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestRefreshPanicEmitsFailedFinishAndPreservesRecovery(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	started := make(chan struct{})
	release := make(chan struct{})
	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		close(started)
		<-release
		panic("controlled refresh panic")
	})
	idx.debounce = 0
	idx.refreshAsync(t.Context(), RefreshReasonPeriodic)
	<-started
	idx.mu.Lock()
	run := idx.refreshRun
	idx.mu.Unlock()
	close(release)
	<-run.done
	if run.err == nil {
		t.Fatal("background recovery did not record the panic")
	}

	events := decodeRefreshEvents(t, output.Bytes())
	if len(events) != 2 {
		t.Fatalf("events = %d, want start and finish: %s", len(events), output.String())
	}
	finish := events[1]
	if finish.Message != "conversation.index.refresh_finished" || finish.Outcome != "failed" || finish.CacheChanged {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestCachedRequestHookAndTerminalExportSkipProviderDiscovery(t *testing.T) {
	discoveries := &atomic.Int64{}
	parser := &cachedRefreshParser{discoveries: discoveries, record: nil}
	record := testRecord("cursor:cached", ProviderCursor, "cached", "cached conversation")
	record.LatestRequestID = "0e3f0000-0000-4000-8000-000000000000"
	installed := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{changed: true, records: []Record{record}}, nil
	}, parser)
	if err := installed.RefreshWithReason(t.Context(), RefreshReasonInstall); err != nil {
		t.Fatal(err)
	}
	idx := testIndex(t, scan, parser)
	idx.cachePath = installed.cachePath

	resolved, err := idx.Resolve(t.Context(), record.LatestRequestID)
	if err != nil || resolved.ID != record.ID {
		t.Fatalf("cached hook resolution = %q, %v", resolved.ID, err)
	}
	resolved, err = idx.Resolve(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("cached terminal resolution: %v", err)
	}
	body, err := idx.Export(resolved, ExportOptions{
		Format:     ExportFormatMarkdown,
		Whitespace: WhitespacePreserve,
		Content:    NewContentKindSet(ContentKindChat),
	})
	if err != nil || !strings.Contains(string(body), "cached export") {
		t.Fatalf("cached terminal export = %q, %v", string(body), err)
	}
	if discoveries.Load() != 0 {
		t.Fatalf("provider discoveries = %d, want 0", discoveries.Load())
	}
}

type refreshEvent struct {
	Message      string                     `json:"msg"`
	Reason       string                     `json:"reason"`
	Provider     string                     `json:"provider"`
	DurationMS   int64                      `json:"duration_ms"`
	Outcome      string                     `json:"outcome"`
	RecordCount  int                        `json:"record_count"`
	CacheChanged bool                       `json:"cache_changed"`
	Fields       map[string]json.RawMessage `json:"-"`
}

func decodeRefreshEvents(t *testing.T, body []byte) []refreshEvent {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	events := make([]refreshEvent, 0, 2)
	for {
		var fields map[string]json.RawMessage
		err := decoder.Decode(&fields)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		var event refreshEvent
		if err := json.Unmarshal(encoded, &event); err != nil {
			t.Fatal(err)
		}
		event.Fields = fields
		if event.Message == "conversation.index.refresh_started" || event.Message == "conversation.index.refresh_finished" {
			events = append(events, event)
		}
	}
	return events
}

type cachedRefreshParser struct {
	discoveries *atomic.Int64
	record      *Record
}

func (*cachedRefreshParser) Provider() providerid.Provider {
	return providerid.ProviderCursor
}

func (parser *cachedRefreshParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	parser.discoveries.Add(1)
	if parser.record != nil {
		return []ScanCandidate{{Path: parser.record.ArtifactPath, Stamp: FileStamp{Size: 1}}}, nil
	}
	return nil, nil
}

func (parser *cachedRefreshParser) ScanRecord(string, FileStamp) (Record, bool) {
	if parser.record != nil {
		return *parser.record, true
	}
	return emptyRecord(), false
}

func (*cachedRefreshParser) Stream(string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		yield(exportChatMessage("cached", "user", "cached export", 1), nil)
	}
}
