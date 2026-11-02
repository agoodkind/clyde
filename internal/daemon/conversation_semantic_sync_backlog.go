package daemon

import "context"

// runBacklog repeats passes until the context ends or the needed count becomes zero.
// After the first pass, it also stops if a pass neither reduces the needed count
// nor completes a delivery. The worker retries remaining conversations at the
// next interval.
func (w *conversationSemanticSyncWorker) runBacklog(ctx context.Context) {
	previousNeeded := -1
	for {
		w.lastNeeded = 0
		w.lastCompleted = 0
		w.lastSliced = false
		w.runPassAndLog(ctx)
		needed := w.lastNeeded
		if semanticSyncContextDone(ctx) || needed == 0 {
			return
		}
		if previousNeeded >= 0 && needed >= previousNeeded && w.lastCompleted == 0 && !w.lastSliced {
			return
		}
		previousNeeded = needed
	}
}
