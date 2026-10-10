package daemon_test

import (
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
)

func statusProofSyncIdle(semantic daemon.SemanticStatus) bool {
	return statusProofText(semantic.Detail, "semantic_sync.state") == "idle"
}

func runStatusProofFirstIngestionStep(t *testing.T) *daemon.RuntimeStatus {
	t.Helper()
	waitForIngestionProofFreshness(t, "a", ingestionProofPassTimeout, func(freshness conversation.SearchFreshness) bool {
		return freshness.LastSyncUnix > 0 && freshness.Needed == 0 && freshness.Pending == 0 && freshness.Embedded == 1
	})
	afterFirst := waitForStatusProof(t, "a", ingestionProofPassTimeout, func(semantic daemon.SemanticStatus) bool {
		sent, _ := statusProofKnownInt(semantic.Detail, "semantic_sync.totals.sent_conversations")
		_, upserted := statusProofKnownInt(semantic.Detail, "semantic_sync.last_upsert.rows_written")
		_, checkpointed := statusProofKnownInt(semantic.Detail, "semantic_sync.checkpoint.count")
		return statusProofSyncIdle(semantic) && sent == 1 && upserted && checkpointed
	})
	semantic := afterFirst.Semantic
	detail := semantic.Detail
	if statusProofInt(t, "a", detail, "semantic_sync.totals.documents") <= 0 ||
		statusProofInt(t, "a", detail, "semantic_sync.totals.failed") != 0 ||
		statusProofInt(t, "a", detail, "semantic_sync.totals.failed_suppressed") != 0 {
		t.Errorf("step=a totals=%+v want_documents>0 want_failed=0 want_failed_suppressed=0", statusProofMetricsWithPrefix(detail, "semantic_sync.totals."))
	}
	if statusProofInt(t, "a", detail, "semantic_sync.passes") <= 0 {
		t.Errorf("step=a passes=%d want>0", statusProofInt(t, "a", detail, "semantic_sync.passes"))
	}
	statusProofInt(t, "a", detail, "semantic_sync.last_pass.sent_conversations")
	statusProofInt(t, "a", detail, "semantic_sync.last_pass_duration")
	assertStatusProofTexts(t, "a", detail, "semantic_sync.last_pass_started", "semantic_sync.last_upsert.completed")
	if statusProofInt(t, "a", detail, "semantic_sync.last_upsert.rows_written") <= 0 ||
		statusProofInt(t, "a", detail, "semantic_sync.last_upsert.vectors_embedded") <= 0 {
		t.Errorf("step=a last_upsert=%+v want_rows_written>0 want_vectors_embedded>0", statusProofMetricsWithPrefix(detail, "semantic_sync.last_upsert."))
	}
	if rate := statusProofNumber(t, "a", detail, "semantic_sync.last_upsert.vectors_embedded_rate"); rate <= 0 {
		t.Errorf("step=a vectors_embedded_rate=%v want>0", rate)
	}
	for _, counter := range []string{"rows_written", "vectors_reused", "vectors_embedded"} {
		total := statusProofInt(t, "a", detail, "semantic_sync.upsert_totals."+counter)
		last := statusProofInt(t, "a", detail, "semantic_sync.last_upsert."+counter)
		if total != last {
			t.Errorf("step=a counter=%s upsert_total=%d last_upsert=%d", counter, total, last)
		}
	}
	collectionName := statusProofText(detail, "semantic_settings.collection_name")
	directory := statusProofText(detail, "semantic_sync.checkpoint.directory")
	if statusProofInt(t, "a", detail, "semantic_sync.checkpoint.count") != 1 || collectionName == "" || !strings.HasSuffix(directory, collectionName) {
		t.Errorf("step=a checkpoint_directory=%q collection_name=%q want_count=1", directory, collectionName)
	}
	assertStatusProofUnknown(t, "a", detail, "semantic_sync.active_job_id", "semantic_sync.last_error.text", "semantic_sync.last_error.time")
	if !semantic.IngestionEnabled || !semantic.SearchEnabled || semantic.Backend != "local" || semantic.Connection != "ready" ||
		semantic.NextRetryUnix != nil || semantic.Attempts != 1 {
		t.Errorf("step=a semantic=%+v", semantic)
	}
	assertStatusProofLocalSettings(t, detail)
	process := afterFirst.Process
	if statusProofInt(t, "a", process, "process.goroutines") <= 0 || statusProofInt(t, "a", process, "process.heap_in_use") <= 0 {
		t.Errorf("step=a process=%+v", process)
	}
	assertStatusProofTexts(t, "a", process, "process.started")
	return afterFirst
}

func assertStatusProofLocalSettings(t *testing.T, detail []daemon.StatusMetric) {
	t.Helper()
	if statusProofText(detail, "semantic_settings.collection_id") != statusProofDefaultCollectionID ||
		statusProofText(detail, "semantic_settings.backend") != "local" ||
		statusProofInt(t, "a", detail, "semantic_settings.sync_interval") != statusProofIntervalMS ||
		statusProofInt(t, "a", detail, "semantic_settings.index_refresh_interval") != statusProofIntervalMS {
		t.Errorf("step=a settings=%+v", statusProofMetricsWithPrefix(detail, "semantic_settings."))
	}
	assertStatusProofTexts(t, "a", detail, "semantic_settings.collection_name", "semantic_settings.embedding_model")
	if statusProofInt(t, "a", detail, "semantic_settings.embedding_dimension") <= 0 {
		t.Errorf("step=a settings=%+v want_embedding_dimension>0", statusProofMetricsWithPrefix(detail, "semantic_settings."))
	}
	assertStatusProofMissing(t, "a", detail,
		"semantic_settings.embedding_base_url", "semantic_settings.milvus_address", "semantic_settings.milvus_database")
}

func runStatusProofUnchangedPassStep(t *testing.T, afterFirst *daemon.RuntimeStatus) {
	t.Helper()
	if afterFirst == nil {
		t.Fatalf("step=b first_status=nil")
	}
	before := afterFirst.Semantic.Detail
	passesBefore := statusProofInt(t, "b", before, "semantic_sync.passes")
	later := waitForStatusProof(t, "b", ingestionProofPassTimeout, func(semantic daemon.SemanticStatus) bool {
		passes, known := statusProofKnownInt(semantic.Detail, "semantic_sync.passes")
		return statusProofSyncIdle(semantic) && known && passes > passesBefore+1
	})
	after := later.Semantic.Detail
	unchanged := []string{"semantic_sync.totals.", "semantic_sync.upsert_totals.", "semantic_sync.last_upsert."}
	wantUnchanged := statusProofMetricsWithPrefix(before, unchanged...)
	gotUnchanged := statusProofMetricsWithPrefix(after, unchanged...)
	if len(wantUnchanged) == 0 || !slices.Equal(gotUnchanged, wantUnchanged) {
		t.Errorf("step=b metrics=%+v want=%+v", gotUnchanged, wantUnchanged)
	}
	for _, counter := range []string{"sent_conversations", "documents", "failed"} {
		if value := statusProofInt(t, "b", after, "semantic_sync.last_pass."+counter); value != 0 {
			t.Errorf("step=b last_pass_counter=%s value=%d want=0", counter, value)
		}
	}
}

func runStatusProofCommandStep(t *testing.T) {
	t.Helper()
	text := runStatusProofCommand(t, "c")
	assertStatusProofLines(t, "c", text,
		"daemon.responding true",
		"semantic.ingestion_enabled true",
		"semantic.search_enabled true",
		"semantic.backend local",
		"semantic.connection ready",
		"semantic.attempts 1 attempts",
		"semantic.next_retry null",
		"semantic_settings.backend local",
		"semantic_settings.collection_id "+statusProofDefaultCollectionID,
		"semantic_settings.sync_interval 2000 ms",
		"semantic_sync.state idle",
		"semantic_sync.active_job_id null",
		"semantic_sync.totals.sent_conversations 1 conversations",
		"semantic_sync.totals.failed 0 conversations",
		"semantic_sync.last_pass.sent_conversations 0 conversations",
		"semantic_sync.last_error.text null",
		"semantic_sync.checkpoint.count 1 checkpoints",
		"semantic_freshness.pending 0 conversations",
	)
	document, _ := statusProofJSON(t, "c")
	assertStatusProofJSONValue(t, "c", document, "daemon.responding", "true")
	assertStatusProofJSONValue(t, "c", document, "semantic.backend", `"local"`)
	assertStatusProofJSONValue(t, "c", document, "semantic.next_retry", statusProofJSONNull)
	assertStatusProofJSONValue(t, "c", document, "semantic_sync.state", `"idle"`)
	assertStatusProofJSONValue(t, "c", document, "semantic_sync.active_job_id", statusProofJSONNull)
	assertStatusProofJSONValue(t, "c", document, "semantic_sync.totals.sent_conversations", "1")
	assertStatusProofJSONValue(t, "c", document, "semantic_sync.checkpoint.count", "1")
	assertStatusProofJSONValue(t, "c", document, "semantic_freshness.manifest", "1")
	for _, name := range []string{"semantic_settings.embedding_base_url", "semantic_settings.milvus_address"} {
		if _, found := document.Counters[name]; found || strings.Contains(text, name) {
			t.Errorf("step=c counter=%s want_missing", name)
		}
	}
	if unit := document.Counters["semantic_sync.totals.sent_conversations"].Unit; unit != "conversations" {
		t.Errorf("step=c counter=semantic_sync.totals.sent_conversations unit=%q want=conversations", unit)
	}
	if _, found := document.Identity["status.read_at"]; !found {
		t.Errorf("step=c identity=%v want_status_read_at", document.Identity)
	}
	if len(document.Notices) != 0 || len(document.Activity) != 0 {
		t.Errorf("step=c notices=%v activity=%v want_empty", document.Notices, document.Activity)
	}
}
