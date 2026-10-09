package daemon

import (
	"time"

	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

type semanticIngestStatusReporter interface {
	IngestStatus(collectionID string) vectorsearch.IngestStatus
}

type semanticSyncPassCounters struct {
	sentConversations int64
	documents         int64
	failed            int64
	failedSuppressed  int64
	deferred          int64
	policySkipped     int64
	unchangedPinned   int64
}

type semanticSyncUpsertCounters struct {
	rowsWritten     int64
	vectorsReused   int64
	vectorsEmbedded int64
}

type semanticSyncPassState string

const (
	semanticSyncPassStateIdle    semanticSyncPassState = "idle"
	semanticSyncPassStateRunning semanticSyncPassState = "running"
	semanticSyncPassStateBlocked semanticSyncPassState = "blocked"
	semanticSyncPassStateFailed  semanticSyncPassState = "failed"
)

type conversationSemanticSyncStatus struct {
	processStarted time.Time
	started        bool
	state          semanticSyncPassState
	activeJobID    string

	lastPassStarted       time.Time
	lastPassDuration      time.Duration
	lastPassDurationKnown bool
	lastPass              semanticSyncPassCounters
	lastPassKnown         bool
	passes                int64
	totals                semanticSyncPassCounters

	lastUpsert          semanticSyncUpsertCounters
	lastUpsertKnown     bool
	lastUpsertCompleted time.Time
	lastUpsertDuration  time.Duration
	upsertTotals        semanticSyncUpsertCounters
	upsertTotalsKnown   bool

	lastErrorText string
	lastErrorAt   time.Time

	embeddingModel      string
	embeddingDimension  int
	checkpointDirectory string
	checkpointCount     int
	checkpointKnown     bool
}

func newConversationSemanticSyncStatus(processStarted time.Time) conversationSemanticSyncStatus {
	return conversationSemanticSyncStatus{
		processStarted:        processStarted,
		started:               false,
		state:                 semanticSyncPassStateIdle,
		activeJobID:           "",
		lastPassStarted:       time.Time{},
		lastPassDuration:      0,
		lastPassDurationKnown: false,
		lastPass:              zeroSemanticSyncPassCounters(),
		lastPassKnown:         false,
		passes:                0,
		totals:                zeroSemanticSyncPassCounters(),
		lastUpsert:            zeroSemanticSyncUpsertCounters(),
		lastUpsertKnown:       false,
		lastUpsertCompleted:   time.Time{},
		lastUpsertDuration:    0,
		upsertTotals:          zeroSemanticSyncUpsertCounters(),
		upsertTotalsKnown:     false,
		lastErrorText:         "",
		lastErrorAt:           time.Time{},
		embeddingModel:        "",
		embeddingDimension:    0,
		checkpointDirectory:   "",
		checkpointCount:       0,
		checkpointKnown:       false,
	}
}

func zeroSemanticSyncPassCounters() semanticSyncPassCounters {
	return semanticSyncPassCounters{
		sentConversations: 0,
		documents:         0,
		failed:            0,
		failedSuppressed:  0,
		deferred:          0,
		policySkipped:     0,
		unchangedPinned:   0,
	}
}

func zeroSemanticSyncUpsertCounters() semanticSyncUpsertCounters {
	return semanticSyncUpsertCounters{rowsWritten: 0, vectorsReused: 0, vectorsEmbedded: 0}
}

func (s *conversationSemanticSyncStatus) recordPass(stats conversationSemanticSyncStats) {
	s.lastPass = semanticSyncPassCounters{
		sentConversations: int64(stats.sentConversations),
		documents:         int64(stats.documents),
		failed:            int64(stats.failed),
		failedSuppressed:  int64(stats.failedSuppressed),
		deferred:          int64(stats.deferred),
		policySkipped:     int64(stats.policySkipped),
		unchangedPinned:   int64(stats.unchangedPinned),
	}
	s.lastPassKnown = true
	s.passes++
	s.totals.sentConversations += s.lastPass.sentConversations
	s.totals.documents += s.lastPass.documents
	s.totals.failed += s.lastPass.failed
	s.totals.failedSuppressed += s.lastPass.failedSuppressed
	s.totals.deferred += s.lastPass.deferred
	s.totals.policySkipped += s.lastPass.policySkipped
	s.totals.unchangedPinned += s.lastPass.unchangedPinned
}

func (s *conversationSemanticSyncStatus) recordIngestStatus(reported vectorsearch.IngestStatus) {
	s.embeddingModel = reported.EmbeddingModel
	s.embeddingDimension = reported.EmbeddingDimension
	s.checkpointKnown = reported.CheckpointKnown
	s.checkpointDirectory = reported.CheckpointDirectory
	s.checkpointCount = reported.CheckpointCount
}
