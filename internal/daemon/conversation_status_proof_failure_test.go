package daemon_test

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
)

func runStatusProofFailedLoadStep(t *testing.T) {
	t.Helper()
	failed := waitForStatusProof(t, "d", ingestionProofFailureTimeout, func(semantic daemon.SemanticStatus) bool {
		failures, known := statusProofKnownInt(semantic.Detail, "semantic_sync.totals.failed")
		return known && failures > 0
	}).Semantic.Detail
	assertStatusProofTexts(t, "d", failed, "semantic_sync.last_error.text", "semantic_sync.last_error.time")
	if suppressedTotal := statusProofInt(t, "d", failed, "semantic_sync.totals.failed_suppressed"); suppressedTotal != 0 {
		t.Errorf("step=d totals_failed_suppressed=%d want=0", suppressedTotal)
	}
	suppressed := waitForStatusProof(t, "d", ingestionProofFailureTimeout, func(semantic daemon.SemanticStatus) bool {
		lastSuppressed, suppressedKnown := statusProofKnownInt(semantic.Detail, "semantic_sync.last_pass.failed_suppressed")
		lastFailed, failedKnown := statusProofKnownInt(semantic.Detail, "semantic_sync.last_pass.failed")
		return statusProofSyncIdle(semantic) && suppressedKnown && failedKnown && lastSuppressed == 1 && lastFailed == 0
	}).Semantic.Detail
	if statusProofInt(t, "d", suppressed, "semantic_sync.totals.failed") < statusProofInt(t, "d", failed, "semantic_sync.totals.failed") ||
		statusProofInt(t, "d", suppressed, "semantic_sync.totals.failed_suppressed") <= 0 {
		t.Errorf("step=d totals=%+v", statusProofMetricsWithPrefix(suppressed, "semantic_sync.totals."))
	}
	freshness := waitForIngestionProofFreshness(t, "d", ingestionProofSettleTimeout, func(freshness conversation.SearchFreshness) bool {
		return freshness.Needed == 0 && freshness.Pending == 1
	})
	if freshness.Manifest != freshness.Embedded+1 {
		t.Errorf("step=d freshness=%+v want_manifest=embedded+1", freshness)
	}
	text := runStatusProofCommand(t, "d")
	assertStatusProofLines(t, "d", text,
		"semantic_sync.last_pass.failed 0 conversations",
		"semantic_sync.last_pass.failed_suppressed 1 conversations",
		"semantic_freshness.pending 1 conversations",
		"semantic_freshness.needed 0 conversations",
	)
	if strings.Contains(text, "semantic_sync.last_error.text null") {
		t.Errorf("step=d last_error_text=null body=\n%s", text)
	}
}

func assertStatusProofBlockedDetail(t *testing.T, semantic daemon.SemanticStatus) {
	t.Helper()
	detail := semantic.Detail
	if semantic.Backend != "milvus" || semantic.Connection != "unavailable" || semantic.Attempts == 0 ||
		semantic.NextRetryUnix == nil || *semantic.NextRetryUnix <= time.Now().Unix() {
		t.Errorf("semantic=%+v", semantic)
	}
	assertStatusProofUnknown(t, "blocked", detail,
		"semantic_sync.last_pass.failed", "semantic_sync.last_pass.sent_conversations",
		"semantic_sync.last_upsert.rows_written", "semantic_sync.last_upsert.completed", "semantic_sync.last_upsert.vectors_embedded_rate",
		"semantic_sync.upsert_totals.rows_written", "semantic_sync.checkpoint.count", "semantic_sync.checkpoint.directory",
		"semantic_sync.active_job_id", "semantic_sync.last_error.text", "semantic_sync.last_error.time")
	if passes := statusProofInt(t, "blocked", detail, "semantic_sync.passes"); passes != 0 {
		t.Errorf("passes=%d want=0", passes)
	}
	statusProofInt(t, "blocked", detail, "semantic_sync.last_pass_duration")
	assertStatusProofTexts(t, "blocked", detail, "semantic_sync.last_pass_started")
	if got := statusProofText(detail, "semantic_settings.embedding_base_url"); got != statusProofRedactedBaseURL {
		t.Errorf("embedding_base_url=%q want=%s", got, statusProofRedactedBaseURL)
	}
	if got := statusProofText(detail, "semantic_settings.milvus_address"); got != statusProofRedactedMilvus {
		t.Errorf("milvus_address=%q want=%s", got, statusProofRedactedMilvus)
	}
	if statusProofText(detail, "semantic_settings.collection_id") != statusProofDefaultCollectionID ||
		statusProofText(detail, "semantic_settings.backend") != "milvus" ||
		statusProofText(detail, "semantic_settings.embedding_model") != statusProofDefaultModel ||
		statusProofInt(t, "blocked", detail, "semantic_settings.embedding_dimension") != statusProofDefaultDimension ||
		statusProofText(detail, "semantic_settings.milvus_database") != "default" ||
		statusProofInt(t, "blocked", detail, "semantic_settings.sync_interval") != statusProofIntervalMS ||
		statusProofInt(t, "blocked", detail, "semantic_settings.index_refresh_interval") != statusProofIntervalMS {
		t.Errorf("settings=%+v", statusProofMetricsWithPrefix(detail, "semantic_settings."))
	}
}

func TestConversationStatusReportsBlockedSyncAndRedactedSettings(t *testing.T) {
	startIngestionProofDaemonWithConfig(t, statusProofBlockedDaemonConfig)

	blocked := waitForStatusProof(t, "blocked", ingestionProofFailureTimeout, func(semantic daemon.SemanticStatus) bool {
		return statusProofText(semantic.Detail, "semantic_sync.state") == "blocked" && semantic.NextRetryUnix != nil
	})
	assertStatusProofBlockedDetail(t, blocked.Semantic)

	text := runStatusProofCommand(t, "blocked")
	assertStatusProofLines(t, "blocked", text,
		"semantic.backend milvus",
		"semantic_settings.backend milvus",
		"semantic.connection unavailable",
		"semantic_settings.embedding_base_url "+statusProofRedactedBaseURL,
		"semantic_settings.milvus_address "+statusProofRedactedMilvus,
		"semantic_settings.embedding_model "+statusProofDefaultModel,
		"semantic_settings.embedding_dimension 4096",
		"semantic_sync.state blocked",
		"semantic_sync.passes 0 passes",
		"semantic_sync.last_pass.failed null conversations",
		"semantic_sync.last_upsert.rows_written null rows",
		"semantic_sync.last_upsert.vectors_embedded_rate null vectors_per_second",
		"semantic_sync.upsert_totals.vectors_embedded null vectors",
		"semantic_sync.checkpoint.directory null",
		"semantic_sync.checkpoint.count null checkpoints",
		"semantic_sync.totals.failed 0 conversations",
	)
	document, body := statusProofJSON(t, "blocked")
	assertStatusProofJSONValue(t, "blocked", document, "semantic_sync.state", `"blocked"`)
	assertStatusProofJSONValue(t, "blocked", document, "semantic_sync.last_pass.failed", statusProofJSONNull)
	assertStatusProofJSONValue(t, "blocked", document, "semantic_sync.last_upsert.rows_written", statusProofJSONNull)
	assertStatusProofJSONValue(t, "blocked", document, "semantic_sync.checkpoint.count", statusProofJSONNull)
	assertStatusProofJSONValue(t, "blocked", document, "semantic_sync.totals.failed", "0")
	assertStatusProofJSONValue(t, "blocked", document, "semantic_settings.embedding_base_url", `"`+statusProofRedactedBaseURL+`"`)
	assertStatusProofJSONValue(t, "blocked", document, "semantic_settings.milvus_address", `"`+statusProofRedactedMilvus+`"`)
	for _, rendered := range []string{text, body} {
		for _, secret := range []string{statusProofUser, statusProofPass, statusProofToken} {
			if strings.Contains(rendered, secret) {
				t.Errorf("rendered_status_contains=%s", secret)
			}
		}
	}
}
