package daemon

import "context"

// unchangedPinnedLogIDs limits the number of IDs in each unchanged_pinned log record.
const unchangedPinnedLogIDs = 100

// logPass publishes freshness and writes one pass_completed record.
// The record uses Info when the pass sends, withholds, suppresses, or pins a
// conversation, fails a delivery, or first lists a conversation.
// Other passes use Debug.
// The Info record reports policy_skipped and failed_suppressed beside failed.
// A failed_suppressed conversation has content that the store does not have.
// A pass with pinned conversations also writes one unchanged_pinned record
// with a limited number of conversation IDs.
func (w *conversationSemanticSyncWorker) logPass(ctx context.Context, stats conversationSemanticSyncStats) {
	w.freshness.publish(stats)
	if stats.sentConversations > 0 || stats.failed > 0 || stats.policySkipped > 0 || stats.failedSuppressed > 0 || stats.unchangedPinned > 0 || len(stats.firstListedIDs) > 0 {
		w.log.InfoContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"manifest", stats.manifest,
			"needed", stats.needed,
			"sent_conversations", stats.sentConversations,
			"documents", stats.documents,
			"deferred", stats.deferred,
			"failed", stats.failed,
			"failed_suppressed", stats.failedSuppressed,
			"policy_skipped", stats.policySkipped,
			"injected_stripped", stats.injectedStripped,
			"system_stripped", stats.systemStripped,
			"sent_conversation_ids", boundedConversationIDs(stats.sentConversationIDs),
			"first_listed_conversation_ids", boundedConversationIDs(stats.firstListedIDs),
			"unchanged_pinned", stats.unchangedPinned,
		)
		if len(stats.unchangedPinnedIDs) > 0 {
			w.log.InfoContext(ctx, "daemon.conversation_semantic_sync.unchanged_pinned",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_ids", stats.unchangedPinnedIDs[:min(unchangedPinnedLogIDs, len(stats.unchangedPinnedIDs))],
			)
		}
		return
	}
	w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
		"concern", "conversation.semantic",
		"component", "daemon",
		"manifest", stats.manifest,
		"needed", stats.needed,
		"deferred", stats.deferred,
	)
}
