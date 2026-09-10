package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	idx.refreshAsync(ctx)
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
	idx.refreshAsync(t.Context())
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
	records, _, states, err := readCache(idx.cachePath)
	if err != nil || len(records) != 1 || records[0].Title != "renamed" || !records[0].Archived || states[path].CompleteOffset != 8 {
		t.Fatalf("metadata was not persisted: records=%+v states=%+v err=%v", records, states, err)
	}
	parser.candidate.MetadataChanged = false
	parser.candidate.Stamp.Size = 12
	parser.result.CompleteOffset = 12
	if err := idx.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, _, states, err = readCache(idx.cachePath)
	if err != nil || states[path].CompleteOffset != 12 {
		t.Fatalf("append progress was not persisted: states=%+v err=%v", states, err)
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
	records, stamps, states, err := readCache(idx.cachePath)
	if err != nil || len(records) != 0 || len(stamps) != 0 || len(states) != 0 {
		t.Fatalf("removed source remained in cache: records=%+v stamps=%+v states=%+v err=%v", records, stamps, states, err)
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
