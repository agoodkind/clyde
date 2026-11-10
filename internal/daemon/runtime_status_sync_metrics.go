package daemon

const (
	statusUnitConversations    = "conversations"
	statusUnitDocuments        = "documents"
	statusUnitMessages         = "messages"
	statusUnitRows             = "rows"
	statusUnitVectors          = "vectors"
	statusUnitVectorsPerSecond = "vectors_per_second"
	statusUnitPasses           = "passes"
	statusUnitCheckpoints      = "checkpoints"
)

func semanticSyncPassCounterMetrics(prefix string, counters semanticSyncPassCounters, known bool) []StatusMetric {
	return []StatusMetric{
		knownIntMetric(prefix+"sent_conversations", counters.sentConversations, statusUnitConversations, known),
		knownIntMetric(prefix+"documents", counters.documents, statusUnitDocuments, known),
		knownIntMetric(prefix+"failed", counters.failed, statusUnitConversations, known),
		knownIntMetric(prefix+"failed_suppressed", counters.failedSuppressed, statusUnitConversations, known),
		knownIntMetric(prefix+"deferred", counters.deferred, statusUnitConversations, known),
		knownIntMetric(prefix+"policy_skipped", counters.policySkipped, statusUnitMessages, known),
		knownIntMetric(prefix+"unchanged_pinned", counters.unchangedPinned, statusUnitConversations, known),
	}
}

func semanticSyncUpsertCounterMetrics(prefix string, counters semanticSyncUpsertCounters, known bool) []StatusMetric {
	return []StatusMetric{
		knownIntMetric(prefix+"rows_written", counters.rowsWritten, statusUnitRows, known),
		knownIntMetric(prefix+"vectors_reused", counters.vectorsReused, statusUnitVectors, known),
		knownIntMetric(prefix+"vectors_embedded", counters.vectorsEmbedded, statusUnitVectors, known),
	}
}

func semanticSyncMetrics(syncStatus conversationSemanticSyncStatus) []StatusMetric {
	if !syncStatus.started {
		return nil
	}
	rate := absentMetric("semantic_sync.last_upsert.vectors_embedded_rate", statusUnitVectorsPerSecond)
	if syncStatus.lastUpsertKnown && syncStatus.lastUpsertDuration > 0 {
		embeddedPerSecond := float64(syncStatus.lastUpsert.vectorsEmbedded) / syncStatus.lastUpsertDuration.Seconds()
		rate = floatMetric(rate.Name, embeddedPerSecond, statusUnitVectorsPerSecond)
	}
	errorKnown := syncStatus.lastErrorText != ""
	sync := []StatusMetric{
		textMetric("semantic_sync.state", string(syncStatus.state)),
		knownTextMetric("semantic_sync.active_job_id", syncStatus.activeJobID, syncStatus.activeJobID != ""),
		intMetric("semantic_sync.passes", syncStatus.passes, statusUnitPasses),
		timeMetric("semantic_sync.last_pass_started", syncStatus.lastPassStarted, true),
		knownIntMetric("semantic_sync.last_pass_duration", syncStatus.lastPassDuration.Milliseconds(), statusUnitMilliseconds, syncStatus.lastPassDurationKnown),
		timeMetric("semantic_sync.last_upsert.completed", syncStatus.lastUpsertCompleted, syncStatus.lastUpsertKnown),
		knownIntMetric("semantic_sync.last_upsert.duration", syncStatus.lastUpsertDuration.Milliseconds(), statusUnitMilliseconds, syncStatus.lastUpsertKnown),
		rate,
		knownTextMetric("semantic_sync.last_error.text", syncStatus.lastErrorText, errorKnown),
		timeMetric("semantic_sync.last_error.time", syncStatus.lastErrorAt, errorKnown),
		knownTextMetric("semantic_sync.checkpoint.directory", syncStatus.checkpointDirectory, syncStatus.checkpointKnown && syncStatus.checkpointDirectory != ""),
		knownIntMetric("semantic_sync.checkpoint.count", int64(syncStatus.checkpointCount), statusUnitCheckpoints, syncStatus.checkpointKnown),
	}
	sync = append(sync, semanticSyncPassCounterMetrics("semantic_sync.last_pass.", syncStatus.lastPass, syncStatus.lastPassKnown)...)
	sync = append(sync, semanticSyncPassCounterMetrics("semantic_sync.totals.", syncStatus.totals, true)...)
	sync = append(sync, semanticSyncUpsertCounterMetrics("semantic_sync.last_upsert.", syncStatus.lastUpsert, syncStatus.lastUpsertKnown)...)
	return append(sync, semanticSyncUpsertCounterMetrics("semantic_sync.upsert_totals.", syncStatus.upsertTotals, syncStatus.upsertTotalsKnown)...)
}
