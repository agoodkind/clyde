package daemon

import (
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

func (f *conversationSemanticFreshness) workerStarted() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.started = true
	f.status.state = semanticSyncPassStateIdle
}

func (f *conversationSemanticFreshness) passStarted(started time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.state = semanticSyncPassStateRunning
	f.status.activeJobID = ""
	f.status.lastPassStarted = started
}

func (f *conversationSemanticFreshness) passFinished(
	state semanticSyncPassState,
	duration time.Duration,
	activeJobID string,
	passErr error,
	client conversationSemanticClient,
	collectionID string,
) {
	if f == nil {
		return
	}
	reporter, reports := client.(semanticIngestStatusReporter)
	var reported vectorsearch.IngestStatus
	if reports {
		reported = reporter.IngestStatus(collectionID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.state = state
	f.status.activeJobID = activeJobID
	f.status.lastPassDuration = duration
	f.status.lastPassDurationKnown = true
	if passErr != nil {
		f.status.lastErrorText = passErr.Error()
		f.status.lastErrorAt = clock.Now()
	}
	if reports {
		f.status.recordIngestStatus(reported)
	}
}

func (f *conversationSemanticFreshness) recordSyncError(err error) {
	if f == nil || err == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.lastErrorText = err.Error()
	f.status.lastErrorAt = clock.Now()
}

func (f *conversationSemanticFreshness) recordUpsert(
	client conversationSemanticClient,
	collectionID string,
	jobID string,
	started time.Time,
	completed time.Time,
) {
	if f == nil {
		return
	}
	reporter, reports := client.(semanticIngestStatusReporter)
	if !reports {
		return
	}
	reported := reporter.IngestStatus(collectionID)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.recordIngestStatus(reported)
	if !reported.LastUpsertKnown || reported.LastUpsert.JobID != jobID {
		return
	}
	f.status.lastUpsert = semanticSyncUpsertCounters{
		rowsWritten:     int64(reported.LastUpsert.RowsWritten),
		vectorsReused:   int64(reported.LastUpsert.VectorsReused),
		vectorsEmbedded: int64(reported.LastUpsert.VectorsEmbedded),
	}
	f.status.lastUpsertKnown = true
	f.status.lastUpsertCompleted = completed
	f.status.lastUpsertDuration = completed.Sub(started)
	f.status.upsertTotals.rowsWritten += f.status.lastUpsert.rowsWritten
	f.status.upsertTotals.vectorsReused += f.status.lastUpsert.vectorsReused
	f.status.upsertTotals.vectorsEmbedded += f.status.lastUpsert.vectorsEmbedded
	f.status.upsertTotalsKnown = true
}

func (f *conversationSemanticFreshness) syncStatus() conversationSemanticSyncStatus {
	if f == nil {
		return newConversationSemanticSyncStatus(time.Time{})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (w *conversationSemanticSyncWorker) beginPassStatus() time.Time {
	started := clock.Now()
	w.passBlocked = false
	w.freshness.passStarted(started)
	return started
}

func (w *conversationSemanticSyncWorker) finishPassStatus(started time.Time, passErr error) {
	state := semanticSyncPassStateIdle
	activeJobID := ""
	if passErr != nil {
		state = semanticSyncPassStateFailed
	} else if w.passBlocked {
		state = semanticSyncPassStateBlocked
		activeJobID = w.activeJobID
	}
	var client conversationSemanticClient
	if w.resolveClient != nil {
		client = w.resolveClient()
	}
	w.freshness.passFinished(state, clock.Now().Sub(started), activeJobID, passErr, client, w.collectionID)
}
