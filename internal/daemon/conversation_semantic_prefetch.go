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

type deliveryBatchSizer interface {
	DeliveryBatchBytes() int64
}

func deliveryBatchBytesFor(client conversationSemanticClient) int64 {
	if sizer, ok := client.(deliveryBatchSizer); ok {
		if size := sizer.DeliveryBatchBytes(); size > 0 {
			return size
		}
	}
	return conversationSemanticBatchBytes
}

const maxLoggedConversationIDs = 10

func boundedConversationIDs(ids []string) []string {
	if len(ids) <= maxLoggedConversationIDs {
		return ids
	}
	return ids[:maxLoggedConversationIDs]
}

// Resume after the last delivered ID to give later IDs a turn in the next pass.
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

// Prefetch uses the admission order, deferral rules and transcript budget of
// collectNeededDocuments. collectNeededDocuments loads additional candidates
// after a prefetched conversation returns an empty document set.
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
