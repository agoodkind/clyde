package daemon

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"

	"goodkind.io/clyde/internal/conversation"
)

// deliveryBatchSizer is a feeder client that sets the raw transcript size one
// sync pass loads. The in-process local backend sends no wire request and sets
// a larger size than the default.
type deliveryBatchSizer interface {
	DeliveryBatchBytes() int64
}

// deliveryBatchBytesFor returns the client's batch size, or
// conversationSemanticBatchBytes when the client sets none.
func deliveryBatchBytesFor(client conversationSemanticClient) int64 {
	if sizer, ok := client.(deliveryBatchSizer); ok {
		if size := sizer.DeliveryBatchBytes(); size > 0 {
			return size
		}
	}
	return conversationSemanticBatchBytes
}

// maxLoggedConversationIDs bounds the id list on the pass log line. A backlog
// pass delivers hundreds of conversations. The line lists the first few beside
// the sent_conversations count.
const maxLoggedConversationIDs = 10

// boundedConversationIDs returns the first maxLoggedConversationIDs ids in
// delivery order.
func boundedConversationIDs(ids []string) []string {
	if len(ids) <= maxLoggedConversationIDs {
		return ids
	}
	return ids[:maxLoggedConversationIDs]
}

// rotateAfter returns ids rotated to start at the first id greater than cursor,
// wrapping around to the start. An empty cursor, or one at or past every id,
// keeps the original order. Each batch resumes after the previous batch's last
// delivery instead of restarting at the smallest id.
func rotateAfter(ids []string, cursor string) []string {
	if cursor == "" || len(ids) == 0 {
		return ids
	}
	start := sort.SearchStrings(ids, cursor)
	if start < len(ids) && ids[start] == cursor {
		start++
	}
	if start <= 0 || start >= len(ids) {
		return ids
	}
	rotated := make([]string, 0, len(ids))
	rotated = append(rotated, ids[start:]...)
	rotated = append(rotated, ids[:start]...)
	return rotated
}

// loadedConversation is the result of one loadDocs call.
type loadedConversation struct {
	built  SemanticConversationDocuments
	err    error
	loaded bool
}

func (w *conversationSemanticSyncWorker) prefetchedOrLoad(
	ctx context.Context,
	prefetched map[string]loadedConversation,
	record conversation.Record,
) (SemanticConversationDocuments, error) {
	if loaded, found := prefetched[record.ID]; found {
		return loaded.built, loaded.err
	}
	return w.loadDocs(ctx, record)
}

// prefetchNeededDocuments loads in parallel the conversations that
// collectNeededDocuments admits when every load returns documents: the same
// order, deferral, and byte budget. collectNeededDocuments loads any
// conversation missing from the result itself.
func (w *conversationSemanticSyncWorker) prefetchNeededDocuments(
	ctx context.Context,
	ordered []string,
	recordsByID map[string]conversation.Record,
	stampsByID map[string]conversation.FileStamp,
) map[string]loadedConversation {
	candidates := make([]conversation.Record, 0)
	var artifactBytes int64
	for _, conversationID := range ordered {
		record, found := recordsByID[conversationID]
		if !found {
			continue
		}
		stamp, stamped := stampsByID[conversationID]
		if stamped && w.isActivelyGrowing(stamp) {
			continue
		}
		if len(candidates) > 0 && artifactBytes+stamp.Size > w.batchBytes {
			break
		}
		candidates = append(candidates, record)
		artifactBytes += stamp.Size
		if artifactBytes >= w.batchBytes {
			break
		}
	}
	results := make([]loadedConversation, len(candidates))
	var next atomic.Int64
	var group sync.WaitGroup
	for range min(len(candidates), max(runtime.GOMAXPROCS(0)/2, 1)) {
		group.Go(func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					w.log.ErrorContext(ctx, "daemon.conversation_semantic_sync.prefetch_panic",
						"concern", "conversation.semantic",
						"component", "daemon",
						"err", fmt.Sprintf("panic: %v", recovered),
						"stack", string(debug.Stack()),
					)
				}
			}()
			for {
				position := int(next.Add(1) - 1)
				if position >= len(candidates) || semanticSyncContextDone(ctx) {
					return
				}
				built, err := w.loadDocs(ctx, candidates[position])
				results[position] = loadedConversation{built: built, err: err, loaded: true}
			}
		})
	}
	group.Wait()
	prefetched := make(map[string]loadedConversation, len(candidates))
	for position, record := range candidates {
		if results[position].loaded {
			prefetched[record.ID] = results[position]
		}
	}
	return prefetched
}
