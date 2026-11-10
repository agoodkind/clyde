package vectorsearch

import (
	"path/filepath"
	"strings"
)

// UpsertStats applies to one completed upsert.
type UpsertStats struct {
	JobID           string
	RowsWritten     int
	VectorsReused   int
	VectorsEmbedded int
}

// IngestStatus reports ingestion state for one client.
type IngestStatus struct {
	EmbeddingModel      string
	EmbeddingDimension  int
	CheckpointDirectory string
	CheckpointCount     int
	CheckpointKnown     bool
	LastUpsert          UpsertStats
	LastUpsertKnown     bool
}

// IngestStatus reads values from memory without scanning a directory.
func (c *Client) IngestStatus(collectionID string) IngestStatus {
	status := IngestStatus{
		EmbeddingModel:      "",
		EmbeddingDimension:  0,
		CheckpointDirectory: "",
		CheckpointCount:     0,
		CheckpointKnown:     false,
		LastUpsert:          UpsertStats{JobID: "", RowsWritten: 0, VectorsReused: 0, VectorsEmbedded: 0},
		LastUpsertKnown:     false,
	}
	if c == nil {
		return status
	}
	status.EmbeddingModel = c.embeddingModel
	status.EmbeddingDimension = c.dimension
	c.ingestMu.Lock()
	status.LastUpsert = c.lastUpsert
	status.LastUpsertKnown = c.lastUpsertKnown
	c.ingestMu.Unlock()
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" || c.checkpoint == nil {
		return status
	}
	collectionName := CollectionName(trimmedCollectionID)
	status.CheckpointDirectory, status.CheckpointCount, status.CheckpointKnown = c.checkpoint.loadedCount(collectionName)
	return status
}

func (c *Client) recordUpsert(stats UpsertStats) {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	c.lastUpsert = stats
	c.lastUpsertKnown = true
}

func (store *checkpointStore) loadedCount(collectionName string) (string, int, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	fingerprints, found := store.loaded[collectionName]
	if !found {
		return "", 0, false
	}
	directory := ""
	if store.root != "" {
		directory = filepath.Join(store.root, collectionName)
	}
	return directory, len(fingerprints), true
}
