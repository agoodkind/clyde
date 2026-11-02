package daemon

import (
	"slices"
	"sort"

	"goodkind.io/clyde/internal/conversation"
)

// arrivalCohort groups conversations first needed in the same pass and orders
// them by file modification time, newest first.
type arrivalCohort struct {
	sequence int
	ids      []string
}

// deliveryOrder alternates batches between rotated order and arrival cohort order.
// Its boolean result reports whether the batch uses rotated order.
func (w *conversationSemanticSyncWorker) deliveryOrder(
	needed []string,
	stampsByID map[string]conversation.FileStamp,
) ([]string, bool) {
	priorityTurn := w.priorityBatchNext
	w.priorityBatchNext = !priorityTurn
	if priorityTurn {
		return w.priorityOrder(stampsByID), false
	}
	ordered := make([]string, len(needed))
	copy(ordered, needed)
	sort.Strings(ordered)
	return rotateAfter(ordered, w.deliveryCursor), true
}

// priorityOrder interleaves conversations across cohorts, starting with the next
// cohort in the current round. The round includes cohorts present at its start.
// Later cohorts can appear in a batch but do not extend the round. Within each
// cohort, idle conversations precede growing conversations.
func (w *conversationSemanticSyncWorker) priorityOrder(
	stampsByID map[string]conversation.FileStamp,
) []string {
	cohortCount := len(w.arrivalCohorts)
	orderedCohorts := make([][]string, cohortCount)
	inRound := func(cohort arrivalCohort) bool {
		return cohort.sequence > w.lastCohortServed && cohort.sequence <= w.roundLastSequence
	}
	if !slices.ContainsFunc(w.arrivalCohorts, inRound) {
		w.lastCohortServed = 0
		w.roundLastSequence = w.nextCohortSequence
	}
	start := 0
	total := 0
	longest := 0
	for position := cohortCount - 1; position >= 0; position-- {
		cohort := w.arrivalCohorts[position]
		if inRound(cohort) {
			start = position
		}
		orderedCohorts[position] = w.eligibleBeforeDeferred(cohort.ids, stampsByID)
		total += len(cohort.ids)
		longest = max(longest, len(cohort.ids))
	}
	order := make([]string, 0, total)
	for round := range longest {
		for offset := range cohortCount {
			ids := orderedCohorts[(start+offset)%cohortCount]
			if round < len(ids) {
				order = append(order, ids[round])
			}
		}
	}
	return order
}

// eligibleBeforeDeferred returns every ID, with idle conversations before growing
// conversations. It preserves the relative order within each group.
func (w *conversationSemanticSyncWorker) eligibleBeforeDeferred(
	ids []string,
	stampsByID map[string]conversation.FileStamp,
) []string {
	eligible := make([]string, 0, len(ids))
	deferred := make([]string, 0)
	for _, conversationID := range ids {
		if stamp, stamped := stampsByID[conversationID]; stamped && w.isActivelyGrowing(stamp) {
			deferred = append(deferred, conversationID)
			continue
		}
		eligible = append(eligible, conversationID)
	}
	return append(eligible, deferred...)
}

// advanceDeliveryCursor sets the delivery cursor to conversationID only for a
// rotated batch.
func (w *conversationSemanticSyncWorker) advanceDeliveryCursor(rotated bool, conversationID string) {
	if rotated {
		w.deliveryCursor = conversationID
	}
}

// markBatchServed records the cohort of the first sent conversation for an
// arrival cohort batch.
func (w *conversationSemanticSyncWorker) markBatchServed(rotated bool, sent []deliveredConversation) {
	if rotated || len(sent) == 0 {
		return
	}
	for _, cohort := range w.arrivalCohorts {
		if slices.Contains(cohort.ids, sent[0].id) {
			w.lastCohortServed = cohort.sequence
			return
		}
	}
}

func newestModifiedFirst(ids []string, stampsByID map[string]conversation.FileStamp) []string {
	newest := make([]string, len(ids))
	copy(newest, ids)
	sort.Strings(newest)
	sort.SliceStable(newest, func(left, right int) bool {
		return stampsByID[newest[left]].Mtime.After(stampsByID[newest[right]].Mtime)
	})
	return newest
}

// reconcileNeeded removes conversations that the engine no longer needs and
// preserves the cohort and position of each remaining conversation. It creates
// one cohort for newly needed conversations and returns their IDs.
func (w *conversationSemanticSyncWorker) reconcileNeeded(
	needed []string,
	stampsByID map[string]conversation.FileStamp,
) []string {
	listed := make(map[string]bool, len(needed))
	for _, conversationID := range needed {
		listed[conversationID] = true
	}
	queued := make(map[string]bool, len(needed))
	cohorts := make([]arrivalCohort, 0, len(w.arrivalCohorts)+1)
	for _, cohort := range w.arrivalCohorts {
		kept := make([]string, 0, len(cohort.ids))
		for _, conversationID := range cohort.ids {
			if listed[conversationID] {
				kept = append(kept, conversationID)
				queued[conversationID] = true
			}
		}
		if len(kept) > 0 {
			cohorts = append(cohorts, arrivalCohort{sequence: cohort.sequence, ids: kept})
		}
	}
	firstListed := make([]string, 0)
	for _, conversationID := range newestModifiedFirst(needed, stampsByID) {
		if !queued[conversationID] {
			firstListed = append(firstListed, conversationID)
		}
	}
	if len(firstListed) > 0 {
		w.nextCohortSequence++
		cohorts = append(cohorts, arrivalCohort{sequence: w.nextCohortSequence, ids: firstListed})
	}
	w.arrivalCohorts = cohorts
	return firstListed
}
