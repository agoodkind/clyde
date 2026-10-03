package daemon

import "context"

// runBacklog runs passes back to back while each pass lowers the count of
// needed conversations. It returns when no conversation is needed, when a pass
// does not lower the count, or when the context ends. A conversation that stays
// needed, such as one deferred while its transcript grows, waits for the next
// interval.
func (w *conversationSemanticSyncWorker) runBacklog(ctx context.Context) {
	previousNeeded := -1
	for {
		w.lastNeeded = 0
		w.runPassAndLog(ctx)
		needed := w.lastNeeded
		if semanticSyncContextDone(ctx) || needed == 0 {
			return
		}
		if previousNeeded >= 0 && needed >= previousNeeded {
			return
		}
		previousNeeded = needed
	}
}
