package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

// SharedCopyReport is the result of a comparison between a single-collection
// source and its shared copy.
type SharedCopyReport = milvusstore.CopyReport

// SharedCopyProgress reports the rows one copy or verification phase has
// processed and the rows that phase reads.
type SharedCopyProgress = milvusstore.CopyProgress

// SharedCopyProgressFunc receives a [SharedCopyProgress] after each page.
type SharedCopyProgressFunc = milvusstore.ProgressFunc

// sharedCopyPair returns the shared store of the client and the source and
// target collection names. The source must be a single-collection collection
// and the target a shared collection.
func (c *Client) sharedCopyPair(sourceID string, targetID string) (*milvusstore.SharedStore, *milvusBackend, string, string, error) {
	if c == nil {
		return nil, nil, "", "", errors.New("copy conversation collection: client is nil")
	}
	milvus, isMilvus := c.store.(*milvusBackend)
	if !isMilvus || milvus.shared == nil {
		return nil, nil, "", "", errors.New("copy conversation collection: the client does not use the shared Milvus layout")
	}
	source := CollectionName(strings.TrimSpace(sourceID))
	target := CollectionName(strings.TrimSpace(targetID))
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(targetID) == "" {
		return nil, nil, "", "", errors.New("copy conversation collection: source and target collection ids are required")
	}
	if source == target {
		return nil, nil, "", "", fmt.Errorf("copy conversation collection: source and target are both %s", source)
	}
	return milvus.shared, milvus, source, target, nil
}

// prepareSharedCopy loads the source collection, creates each absent target
// collection, gives the target vector collection the source dense index
// definition, enables mmap on both target collections, and loads them.
func (c *Client) prepareSharedCopy(ctx context.Context, milvus *milvusBackend, shared *milvusstore.SharedStore, source string, target string) error {
	exists, err := milvus.loadCollection(ctx, source)
	if err != nil {
		return err
	}
	if !exists {
		return failSharedCopy(ctx, "copy conversation collection: source "+source, ErrCollectionAbsent)
	}
	if err := c.ensureCollection(ctx, target, c.dimension); err != nil {
		return err
	}
	replaced, err := shared.MatchDenseIndex(ctx, source, target)
	if err != nil {
		return failSharedCopy(ctx, "match the dense index of "+target+" to "+source, err)
	}
	changed, err := shared.EnsureMmap(ctx, target)
	if err != nil {
		return failSharedCopy(ctx, "enable mmap on "+target, err)
	}
	if _, err := milvus.collectionPresent(ctx, target); err != nil {
		return err
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.shared_copy_prepared",
		"concern", "conversation.semantic",
		"component", "conversation",
		"source", source,
		"target", target,
		"dense_index_replaced", replaced,
		"mmap_changed", changed,
	)
	return nil
}

// CopyToShared copies the rows of the single-collection source into the shared
// target without embedding. With resume, it copies only the source rows with
// a primary key greater than the largest target primary key. progress receives
// the rows each copy phase has processed after each page. It returns the
// copied row count.
func (c *Client) CopyToShared(ctx context.Context, sourceID string, targetID string, resume bool, progress SharedCopyProgressFunc) (int64, error) {
	shared, milvus, source, target, err := c.sharedCopyPair(sourceID, targetID)
	if err != nil {
		return 0, err
	}
	if err := c.prepareSharedCopy(ctx, milvus, shared, source, target); err != nil {
		return 0, err
	}
	afterID := ""
	if resume {
		afterID, err = shared.MaxOccurrenceID(ctx, target)
		if err != nil {
			return 0, failSharedCopy(ctx, "read the largest copied primary key of "+target, err)
		}
		slog.InfoContext(ctx, "conversation.vectorsearch.shared_copy_resumed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"target", target,
			"after_id", afterID,
		)
	}
	copied, err := shared.CopyFromAfter(ctx, source, target, c.declaration, afterID, progress)
	if err != nil {
		slog.ErrorContext(ctx, "conversation.vectorsearch.shared_copy_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"source", source,
			"target", target,
			"copied", copied.Occurrences,
			"vectors_written", copied.VectorsWritten,
			"err", err,
		)
		return copied.Occurrences, fmt.Errorf("copy %s into %s: %w", source, target, err)
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.shared_copy_written",
		"concern", "conversation.semantic",
		"component", "conversation",
		"target", target,
		"occurrences", copied.Occurrences,
		"vectors_written", copied.VectorsWritten,
	)
	return copied.Occurrences, nil
}

// VerifySharedCopy compares the shared target with the single-collection
// source row by row, including the exact vector bytes. progress receives the
// rows each verification phase has processed after each page.
func (c *Client) VerifySharedCopy(ctx context.Context, sourceID string, targetID string, progress SharedCopyProgressFunc) (SharedCopyReport, error) {
	shared, milvus, source, target, err := c.sharedCopyPair(sourceID, targetID)
	if err != nil {
		return SharedCopyReport{}, err
	}
	if err := c.prepareSharedCopy(ctx, milvus, shared, source, target); err != nil {
		return SharedCopyReport{}, err
	}
	report, err := shared.VerifyCopy(ctx, source, target, c.declaration, progress)
	if err != nil {
		return report, failSharedCopy(ctx, "verify copy of "+source+" in "+target, err)
	}
	return report, nil
}

// RepairSharedCopy copies the missing and mismatched rows that report lists,
// removes the extra rows that report lists, and removes every vector that no
// row references when report counts any.
func (c *Client) RepairSharedCopy(ctx context.Context, sourceID string, targetID string, report SharedCopyReport) error {
	shared, _, source, target, err := c.sharedCopyPair(sourceID, targetID)
	if err != nil {
		return err
	}
	rewrite := make([]string, 0, len(report.Missing)+len(report.Mismatched))
	rewrite = append(rewrite, report.Missing...)
	rewrite = append(rewrite, report.Mismatched...)
	if err := shared.CopyIDs(ctx, source, target, c.declaration, rewrite); err != nil {
		return failSharedCopy(ctx, "repair copy of "+source+" in "+target, err)
	}
	if len(report.Extra) > 0 {
		if _, err := shared.RemoveOccurrences(ctx, target, report.Extra); err != nil {
			return failSharedCopy(ctx, "remove extra rows from "+target, err)
		}
	}
	if report.UnreferencedVectors == 0 {
		return nil
	}
	removed, err := shared.RemoveUnreferencedVectors(ctx, target)
	if err != nil {
		return failSharedCopy(ctx, "remove unreferenced vectors from "+target, err)
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.shared_unreferenced_vectors_removed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"target", target,
		"removed", removed,
	)
	return nil
}

// failSharedCopy logs a failed copy operation and returns it wrapped with the
// operation.
func failSharedCopy(ctx context.Context, operation string, err error) error {
	slog.WarnContext(ctx, "conversation.vectorsearch.shared_copy_operation_failed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("%s: %w", operation, err)
}
