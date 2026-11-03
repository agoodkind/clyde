package vectorsearch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/conversation/semsearch"
)

const (
	// sliceProgressLimit limits the number of partial conversation plans in memory.
	// Without a plan, the client reads stored rows during the next slice.
	sliceProgressLimit = 32
	// defaultSliceProgressBytes limits the memory for one client's retained plans.
	defaultSliceProgressBytes = 128 << 20
	// sliceChunkOverheadBytes estimates memory for a retained chunk's fields beyond its text.
	sliceChunkOverheadBytes = 256
	// sliceKeyEntryBytes estimates memory for one row ID or content key and its map entry.
	sliceKeyEntryBytes = 96
)

// sliceProgress stores one client's plan for a partial conversation between slices.
// The first slice's write record lists every family in the plan. When a slice cannot
// find a planned row, the client discards the plan. The next slice reads stored rows,
// plans again, and records a checkpoint when every row exists. This later plan also
// includes rows from documents that changed between slices.
//
// The plan stores row IDs and the IDs of rows with reusable vectors. Each slice
// generates message text from delivered documents and reads vectors for its next rows.
type sliceProgress struct {
	fingerprint string
	markers     string
	// nextDocument identifies the first delivered document with an unwritten planned row.
	nextDocument int
	// ids lists the unwritten rows that the documents generate.
	ids map[string]struct{}
	// extra lists unwritten rows that the documents do not generate, such as pieces of
	// a rejected input.
	extra []storedChunk
	// repeated lists content keys that appear on multiple planned rows. Later slices
	// can reuse a stored vector from a written row with a matching key.
	repeated map[string]struct{}
	// sources maps a content key to the ID of a stored row with that content.
	sources map[string]string
	// storedSplits lists the IDs of stored split rows in the families with unwritten rows.
	storedSplits map[string]struct{}
	bytes        int64
}

func (progress *sliceProgress) measure() {
	total := int64(len(progress.ids)+len(progress.repeated)+len(progress.sources)+len(progress.storedSplits)) * sliceKeyEntryBytes
	for _, chunk := range progress.extra {
		total += int64(len(chunk.Content)+len(chunk.RelativePath)) + sliceChunkOverheadBytes
	}
	progress.bytes = total
}

// writeMarkers returns a conversation's write record filenames and checkpoint content.
// Another client changes this value when it starts or finishes a write.
// The caller must own the conversation claim.
func (store *checkpointStore) writeMarkers(collectionName string, conversationID string) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.root == "" {
		markers := []string{store.records(collectionName)[conversationID]}
		for _, record := range store.writes[memoryWritesKey(collectionName, conversationID)] {
			markers = append(markers, record.WriteID)
		}
		return strings.Join(markers, "\n"), nil
	}
	entries, err := os.ReadDir(store.writeRecordDirectory(collectionName))
	if err != nil && !os.IsNotExist(err) {
		return "", failRead("read write record directory", err)
	}
	prefix := conversationFileStem(conversationID) + writeRecordSeparator
	markers := make([]string, 0, 1)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && isWriteRecordName(entry.Name()) {
			markers = append(markers, entry.Name())
		}
	}
	checkpoint, err := os.ReadFile(filepath.Join(store.root, collectionName, checkpointFileName(conversationID)))
	if err != nil && !os.IsNotExist(err) {
		return "", failRead("read checkpoint file", err)
	}
	slices.Sort(markers)
	return strings.Join(markers, "\n") + "\n" + string(checkpoint), nil
}

// nextSliceChunks generates up to count planned rows, starting at the first document
// with an unwritten row. It returns each row's document position. After processing
// the documents, it adds pieces from rejected inputs.
func nextSliceChunks(docs []semsearch.SemDoc, byteBudget int, progress *sliceProgress, count int) ([]storedChunk, []int, error) {
	chunks := make([]storedChunk, 0, count)
	positions := make([]int, 0, count)
	seen := make(map[string]struct{}, count)
	for position := progress.nextDocument; position < len(docs); position++ {
		generated, err := documentChunks(docs[position])
		if err != nil {
			return nil, nil, err
		}
		for _, chunk := range expandOverBudget(generated, byteBudget) {
			id := chunkID(chunk)
			if _, planned := progress.ids[id]; !planned {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			chunks = append(chunks, chunk)
			positions = append(positions, position)
			// The limit applies inside one message. A later slice continues the same document.
			if len(chunks) == count {
				return chunks, positions, nil
			}
		}
	}
	for _, chunk := range progress.extra[:min(count-len(chunks), len(progress.extra))] {
		chunks = append(chunks, chunk)
		positions = append(positions, len(docs))
	}
	return chunks, positions, nil
}

// retainedChunk copies a split chunk's text. Without the copy, its substring would
// retain the whole delivered message in memory after the slice returns.
func retainedChunk(chunk storedChunk) storedChunk {
	chunk.Content = strings.Clone(chunk.Content)
	chunk.RelativePath = strings.Clone(chunk.RelativePath)
	return chunk
}

// repeatedKeys returns the content keys that appear more than once in chunks.
func repeatedKeys(chunks []storedChunk) map[string]struct{} {
	counts := make(map[string]int, len(chunks))
	repeated := make(map[string]struct{})
	for _, chunk := range chunks {
		key := contentKey(chunk.Content)
		counts[key]++
		if counts[key] > 1 {
			repeated[key] = struct{}{}
		}
	}
	return repeated
}

// generatedChunkIDs returns the row IDs that the documents generate.
func generatedChunkIDs(docs []semsearch.SemDoc, byteBudget int) (map[string]struct{}, error) {
	generated := make(map[string]struct{})
	for _, doc := range docs {
		chunks, err := documentChunks(doc)
		if err != nil {
			return nil, err
		}
		for _, chunk := range expandOverBudget(chunks, byteBudget) {
			generated[chunkID(chunk)] = struct{}{}
		}
	}
	return generated, nil
}

// saveSliceProgress retains the plan after a partial write. If reading write markers
// fails, it does not retain a plan. The plan does not store generated message text
// or vectors.
func (c *Client) saveSliceProgress(
	collectionName string,
	conversationID string,
	docs []semsearch.SemDoc,
	fingerprint string,
	written []storedChunk,
	unwritten []storedChunk,
	batch *storedBatch,
) {
	markers, err := c.checkpoint.writeMarkers(collectionName, conversationID)
	if err != nil {
		logWriteRecordFailure(conversationID, err)
		return
	}
	generated, err := generatedChunkIDs(docs, c.byteBudget)
	if err != nil {
		logWriteRecordFailure(conversationID, err)
		return
	}
	progress := &sliceProgress{
		fingerprint:  fingerprint,
		markers:      markers,
		nextDocument: 0,
		ids:          make(map[string]struct{}, len(unwritten)),
		extra:        nil,
		repeated:     repeatedKeys(slices.Concat(written, unwritten)),
		sources:      make(map[string]string),
		storedSplits: make(map[string]struct{}),
		bytes:        0,
	}
	state := batch.state(conversationID)
	for _, chunk := range unwritten {
		for _, splitID := range state.splitRowIDs(chunkFamilyKey(conversationID, chunk.RelativePath)) {
			progress.storedSplits[splitID] = struct{}{}
		}
		id := chunkID(chunk)
		if _, found := generated[id]; found {
			progress.ids[id] = struct{}{}
		} else {
			progress.extra = append(progress.extra, retainedChunk(chunk))
		}
		key := contentKey(chunk.Content)
		if source, stored := batch.sourceIDs[key]; stored {
			progress.sources[key] = source
		}
	}
	for _, chunk := range written {
		key := contentKey(chunk.Content)
		if _, later := progress.repeated[key]; later {
			progress.sources[key] = chunkID(chunk)
		}
	}
	c.retainSliceProgress(memoryWritesKey(collectionName, conversationID), progress)
}

// retainSliceProgress retains a plan within the count and byte limits. It removes
// other plans as needed. It does not retain a plan that exceeds the byte limit alone.
// Without a plan, the next slice reads stored rows. The write record still marks the
// conversation for ingestion.
func (c *Client) retainSliceProgress(key string, progress *sliceProgress) {
	progress.measure()
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	delete(c.sliceProgress, key)
	if progress.bytes > c.ingestSlicePlanBytes {
		return
	}
	retained := progress.bytes
	for _, other := range c.sliceProgress {
		retained += other.bytes
	}
	for other, plan := range c.sliceProgress {
		if retained <= c.ingestSlicePlanBytes && len(c.sliceProgress) < sliceProgressLimit {
			break
		}
		retained -= plan.bytes
		delete(c.sliceProgress, other)
	}
	c.sliceProgress[key] = progress
}

func (c *Client) dropSliceProgress(collectionName string, conversationID string) {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	delete(c.sliceProgress, memoryWritesKey(collectionName, conversationID))
}

// currentSliceProgress returns the plan when its fingerprint and write markers match.
// It discards a plan that does not match. The caller must own the conversation claim.
func (c *Client) currentSliceProgress(collectionName string, conversationID string, fingerprint string) *sliceProgress {
	c.ingestMu.Lock()
	progress := c.sliceProgress[memoryWritesKey(collectionName, conversationID)]
	c.ingestMu.Unlock()
	if progress == nil {
		return nil
	}
	markers, err := c.checkpoint.writeMarkers(collectionName, conversationID)
	if err != nil {
		logWriteRecordFailure(conversationID, err)
		c.dropSliceProgress(collectionName, conversationID)
		return nil
	}
	if markers != progress.markers || fingerprint != progress.fingerprint {
		c.dropSliceProgress(collectionName, conversationID)
		return nil
	}
	return progress
}

// readSliceVectors reads reusable stored vectors for the next rows. It selects source
// rows by ID and accepts vectors only for matching content and compatible embedding
// models. The client embeds rows without a usable source.
func (c *Client) readSliceVectors(ctx context.Context, collectionName string, next []storedChunk, progress *sliceProgress) (map[string][]float32, int, error) {
	reuse := make(map[string][]float32)
	milvus, isMilvus := c.store.(*milvusBackend)
	if !isMilvus {
		return reuse, 0, nil
	}
	keyBySource := make(map[string]string)
	sources := make([]string, 0)
	for _, chunk := range next {
		key := contentKey(chunk.Content)
		source, found := progress.sources[key]
		if !found {
			continue
		}
		if _, listed := keyBySource[source]; !listed {
			sources = append(sources, source)
		}
		keyBySource[source] = key
	}
	rows, err := milvus.readRowVectors(ctx, collectionName, sources)
	if err != nil {
		return nil, 0, err
	}
	for source, row := range rows {
		key := keyBySource[source]
		if contentKey(row.content) == key && embeddingModelsCompatible(row.embeddingModel, c.embeddingModel) {
			reuse[key] = row.vector
		}
	}
	return reuse, len(rows), nil
}

// resumeSlice writes the next planned rows and reads only their source vectors.
// The conversation remains incomplete until a later slice reads all stored rows and
// records the checkpoint.
func (c *Client) resumeSlice(
	ctx context.Context,
	collectionName string,
	conversationID string,
	docs []semsearch.SemDoc,
	progress *sliceProgress,
	stats *ingestStats,
	rowLimit int,
) (conversationUpsert, error) {
	result := conversationUpsert{written: 0, processed: 0, complete: false}
	next, positions, err := nextSliceChunks(docs, c.byteBudget, progress, rowLimit)
	if err != nil {
		c.dropSliceProgress(collectionName, conversationID)
		return result, err
	}
	reuse, rowsRead, err := c.readSliceVectors(ctx, collectionName, next, progress)
	stats.rowsRead += rowsRead
	if err != nil {
		c.dropSliceProgress(collectionName, conversationID)
		return result, err
	}
	writer := newRowWriter(c, collectionName)
	storedChild := func(chunk storedChunk) bool {
		_, found := progress.storedSplits[chunkID(chunk)]
		return found
	}
	processed, unwritten, err := c.embedAndWrite(ctx, next, reuse, writer, stats, rowLimit, storedChild)
	result.written = writer.written
	result.processed = processed
	if err != nil || len(next) == 0 {
		c.dropSliceProgress(collectionName, conversationID)
		return result, err
	}
	attempted := make(map[string]bool, len(next))
	for position, chunk := range next {
		id := chunkID(chunk)
		_, planned := progress.ids[id]
		attempted[id] = planned
		delete(progress.ids, id)
		if position < processed {
			key := contentKey(chunk.Content)
			if _, repeated := progress.repeated[key]; repeated {
				progress.sources[key] = id
			}
		}
	}
	progress.extra = slices.DeleteFunc(progress.extra, func(extra storedChunk) bool {
		_, inNext := attempted[chunkID(extra)]
		return inNext
	})
	progress.nextDocument = positions[len(positions)-1]
	if processed < len(positions) {
		progress.nextDocument = positions[processed]
	}
	for _, chunk := range unwritten {
		id := chunkID(chunk)
		if attempted[id] {
			progress.ids[id] = struct{}{}
			continue
		}
		progress.extra = append(progress.extra, retainedChunk(chunk))
	}
	if len(progress.ids)+len(progress.extra) == 0 {
		c.dropSliceProgress(collectionName, conversationID)
		return result, nil
	}
	c.retainSliceProgress(memoryWritesKey(collectionName, conversationID), progress)
	return result, nil
}
