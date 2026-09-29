package conversation

import "context"

// ListAllWithStamps returns every cached record and artifact stamp without a
// refresh, including subagent conversations that the
// conversation.include_subagent_conversations setting hides from ListWithStamps.
// Embedded semantic ingestion applies its own subagent admission setting to
// these records.
func (idx *Index) ListAllWithStamps(ctx context.Context) ([]StampedRecord, error) {
	if err := idx.loadOnce(); err != nil {
		return nil, err
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return cloneStampedRecords(cloneRecords(idx.records), idx.prevStamps), nil
}
