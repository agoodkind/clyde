// Package codestore ranks persisted rows by Hamming distance between 256-bit
// codes. Searches read content and metadata from disk for selected rows.
package codestore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const defaultSearchLimit = 10

// Store loads collection indexes on demand and protects each with a read/write lock.
type Store struct {
	root        string
	model       string
	mutex       sync.Mutex
	collections map[string]*codeCollection
}

var _ collection.Store = (*Store)(nil)

// Open creates the root directory and returns a store.
// The store loads each collection on first use.
func Open(root string, model string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("code store root is required")
	}
	if err := os.MkdirAll(root, directoryMode); err != nil {
		return nil, failed("create code store root "+root, err)
	}
	return &Store{root: root, model: model, mutex: sync.Mutex{}, collections: make(map[string]*codeCollection)}, nil
}

// Close closes every loaded collection and removes it from the store's cache.
func (store *Store) Close() {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	for name, stored := range store.collections {
		stored.mutex.Lock()
		stored.closeFiles()
		stored.mutex.Unlock()
		delete(store.collections, name)
	}
}

func (store *Store) directory(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || trimmed != filepath.Base(trimmed) || trimmed == "." || trimmed == ".." {
		return "", fmt.Errorf("invalid collection name %q", name)
	}
	return filepath.Join(store.root, trimmed), nil
}

func (store *Store) collection(name string) (*codeCollection, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if stored, found := store.collections[name]; found {
		return stored, nil
	}
	dir, err := store.directory(name)
	if err != nil {
		return nil, err
	}
	stored, err := openCollection(dir)
	if err != nil {
		return nil, err
	}
	store.collections[name] = stored
	return stored, nil
}

// Load reads a collection into memory and returns its row count.
// It returns [collection.ErrCollectionMissing] if the header file is absent.
func (store *Store) Load(name string) (int, error) {
	stored, err := store.collection(name)
	if err != nil {
		return 0, err
	}
	stored.mutex.RLock()
	defer stored.mutex.RUnlock()
	return len(stored.rows), nil
}

// EnsureCollection creates a missing collection or adds undeclared scalar columns.
// It rejects dimensions that differ from [staticembed.Dimensions].
func (store *Store) EnsureCollection(ctx context.Context, request collection.EnsureRequest) error {
	if request.Dimension != staticembed.Dimensions {
		return fmt.Errorf("ensure %s: dimension %d, the code store needs %d", request.Collection, request.Dimension, staticembed.Dimensions)
	}
	stored, err := store.collection(request.Collection)
	if errors.Is(err, collection.ErrCollectionMissing) {
		dir, dirErr := store.directory(request.Collection)
		if dirErr != nil {
			return dirErr
		}
		if err := os.MkdirAll(dir, directoryMode); err != nil {
			return failed("create collection "+request.Collection, err)
		}
		header := collectionHeader{Declaration: request.Declaration, Dimension: request.Dimension, Model: store.model, Generation: 0, FrameFormat: frameFormatLengthCRC}
		if err := writeHeader(dir, header); err != nil {
			return err
		}
		_, err = store.collection(request.Collection)
		return err
	}
	if err != nil {
		return err
	}
	stored.mutex.Lock()
	defer stored.mutex.Unlock()
	declaration := stored.header.Declaration
	added := false
	for _, column := range request.Declaration.Scalars {
		if _, found := stored.columnIndex[column.Name]; !found {
			declaration.Scalars = append(declaration.Scalars, column)
			added = true
		}
	}
	if !added {
		return nil
	}
	header := stored.header
	header.Declaration = declaration
	if err := writeHeader(stored.dir, header); err != nil {
		return err
	}
	stored.setDeclaration(declaration)
	for position := range stored.rows {
		missing := len(declaration.Scalars) - len(stored.rows[position].cells)
		stored.rows[position].cells = append(stored.rows[position].cells, make([]uint32, missing)...)
	}
	return nil
}

// Upsert replaces rows by ID and stores their vector sign bits.
// It checks scalar column names against the saved collection declaration.
// The declaration argument does not change that schema.
func (store *Store) Upsert(ctx context.Context, name string, _ collection.Declaration, rows []collection.Row) error {
	if err := ctx.Err(); err != nil {
		return failed("upsert "+name, err)
	}
	stored, err := store.collection(name)
	if err != nil {
		return err
	}
	stored.mutex.Lock()
	defer stored.mutex.Unlock()
	records := make([]rowRecord, 0, len(rows))
	blobs := make([][]byte, 0, len(rows))
	for _, candidate := range rows {
		if strings.TrimSpace(candidate.ID) == "" {
			return fmt.Errorf("upsert %s: row ID is required", name)
		}
		if len(candidate.Vector) != staticembed.Dimensions {
			return fmt.Errorf("upsert %s: row %s has %d dimensions, want %d", name, candidate.ID, len(candidate.Vector), staticembed.Dimensions)
		}
		cells := make([]storedCell, 0, len(candidate.Scalars))
		for columnName, value := range candidate.Scalars {
			column, declared := stored.columnIndex[columnName]
			if !declared {
				return fmt.Errorf("upsert %s: column %q is not declared", name, columnName)
			}
			value.Type = stored.header.Declaration.Scalars[column].Type
			if value.Null {
				value = collection.ScalarValue{Type: value.Type, Null: true, String: "", Bool: false, Int64: 0}
			}
			cells = append(cells, storedCell{column: column, value: value})
		}
		slices.SortFunc(cells, func(left, right storedCell) int { return cmp.Compare(left.column, right.column) })
		records = append(records, rowRecord{
			kind:          recordUpsert,
			id:            candidate.ID,
			code:          staticembed.CodeOf(candidate.Vector),
			relativePath:  candidate.RelativePath,
			splitPart:     candidate.SplitPart,
			splitRecorded: candidate.SplitPartRecorded,
			dataOffset:    0,
			dataLength:    0,
			cells:         cells,
		})
		blobs = append(blobs, encodeBlob(candidate.Content, candidate.Metadata))
	}
	return stored.write(records, blobs)
}

// Delete removes the rows a filter selects.
func (store *Store) Delete(ctx context.Context, name string, filter collection.Filter) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, failed("delete from "+name, err)
	}
	stored, err := store.collection(name)
	if err != nil {
		return 0, err
	}
	stored.mutex.Lock()
	defer stored.mutex.Unlock()
	matches, err := stored.compileFilter(&filter)
	if err != nil {
		return 0, err
	}
	return stored.deleteWhere(func(position int) bool { return matches(position) == truthTrue })
}

func (stored *codeCollection) deleteWhere(selected func(position int) bool) (int64, error) {
	records := make([]rowRecord, 0)
	for position := range stored.rows {
		if !selected(position) {
			continue
		}
		records = append(records, emptyRecord(recordDelete, stored.rows[position].id))
	}
	if len(records) == 0 {
		return 0, nil
	}
	if err := stored.write(records, make([][]byte, len(records))); err != nil {
		return 0, err
	}
	return int64(len(records)), nil
}

func (stored *codeCollection) hit(position int, score float64, columns []collection.ScalarColumn) (collection.Hit, error) {
	content, metadata, err := stored.readBlob(position)
	if err != nil {
		return collection.Hit{}, err
	}
	current := stored.rows[position]
	return collection.Hit{
		ID:                current.id,
		Content:           content,
		Score:             score,
		RelativePath:      current.relativePath,
		StartLine:         0,
		EndLine:           0,
		FileExtension:     "",
		Metadata:          metadata,
		SplitPart:         current.splitPart,
		SplitPartRecorded: current.splitRecorded,
		Scalars:           stored.cells(position, columns),
	}, nil
}

// Query returns the rows a filter selects in ascending ID order.
func (store *Store) Query(ctx context.Context, request collection.QueryRequest) ([]collection.Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, failed("query "+request.Collection, err)
	}
	stored, err := store.collection(request.Collection)
	if err != nil {
		return nil, err
	}
	stored.mutex.RLock()
	defer stored.mutex.RUnlock()
	matches, err := stored.compileFilter(request.Filter)
	if err != nil {
		return nil, err
	}
	positions := stored.selectSorted(func(position int) bool { return matches(position) == truthTrue })
	if request.Limit > 0 && len(positions) > request.Limit {
		positions = positions[:request.Limit]
	}
	hits := make([]collection.Hit, 0, len(positions))
	for _, position := range positions {
		hit, err := stored.hit(position, 0, request.Declaration.Scalars)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

func (stored *codeCollection) selectSorted(selected func(position int) bool) []int {
	positions := make([]int, 0)
	for position := range stored.rows {
		if selected(position) {
			positions = append(positions, position)
		}
	}
	slices.SortFunc(positions, func(left, right int) int { return strings.Compare(stored.rows[left].id, stored.rows[right].id) })
	return positions
}

func (stored *codeCollection) itemSelector(itemColumn string, itemIDs []string, prefixes []string) func(position int) bool {
	wanted := make(map[string]struct{}, len(itemIDs))
	for _, id := range itemIDs {
		wanted[id] = struct{}{}
	}
	column, declared := stored.columnIndex[itemColumn]
	return func(position int) bool {
		current := stored.rows[position]
		if declared {
			if id := current.cells[column]; id != 0 {
				value := stored.dictionaries[column].values[id]
				if _, found := wanted[value.String]; found && !value.Null {
					return true
				}
			}
		}
		for _, prefix := range prefixes {
			if prefix != "" && strings.HasPrefix(current.relativePath, prefix) {
				return true
			}
		}
		return false
	}
}

// QueryRows returns matching items in ascending row ID order.
// The store persists sign codes and returns nil Vector fields.
func (store *Store) QueryRows(ctx context.Context, request collection.RowsRequest) ([]collection.StoredRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, failed("query rows of "+request.Collection, err)
	}
	stored, err := store.collection(request.Collection)
	if err != nil {
		return nil, err
	}
	stored.mutex.RLock()
	defer stored.mutex.RUnlock()
	positions := stored.selectSorted(stored.itemSelector(request.Declaration.ItemIDColumn, request.ItemIDs, request.PathPrefixes))
	rows := make([]collection.StoredRow, 0, len(positions))
	for _, position := range positions {
		content, _, err := stored.readBlob(position)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(content))
		current := stored.rows[position]
		rows = append(rows, collection.StoredRow{
			ID:                current.id,
			RelativePath:      current.relativePath,
			Content:           content,
			SplitPart:         current.splitPart,
			SplitPartRecorded: current.splitRecorded,
			EmbeddingModel:    stored.header.Model,
			ContentHash:       hex.EncodeToString(sum[:]),
			Vector:            nil,
			Scalars:           stored.cells(position, request.Declaration.Scalars),
		})
	}
	return rows, nil
}

// DeleteItems removes rows selected by item IDs or nonempty path prefixes.
func (store *Store) DeleteItems(ctx context.Context, request collection.DeleteItemsRequest) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, failed("delete items from "+request.Collection, err)
	}
	stored, err := store.collection(request.Collection)
	if err != nil {
		return 0, err
	}
	stored.mutex.Lock()
	defer stored.mutex.Unlock()
	return stored.deleteWhere(stored.itemSelector(request.Declaration.ItemIDColumn, request.ItemIDs, request.PathPrefixes))
}

// BackfillScalars applies the supplied backfill rules to stored rows.
// It returns counts of changed rows and unmatched rows that need a backfill.
func (store *Store) BackfillScalars(ctx context.Context, name string, backfill collection.ScalarBackfill) (int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, failed("backfill "+name, err)
	}
	stored, err := store.collection(name)
	if err != nil {
		return 0, 0, err
	}
	stored.mutex.Lock()
	defer stored.mutex.Unlock()
	for _, column := range backfill.Columns {
		if _, declared := stored.columnIndex[column.Name]; !declared {
			return 0, 0, fmt.Errorf("backfill %s: column %q is not declared", name, column.Name)
		}
	}
	changed, orphans := 0, 0
	records := make([]rowRecord, 0)
	for position := range stored.rows {
		record, outcome := stored.backfillRow(position, backfill)
		switch outcome {
		case backfillOrphan:
			orphans++
		case backfillChanged:
			changed++
			if !backfill.DryRun {
				records = append(records, record)
			}
		case backfillUnchanged:
		}
	}
	if len(records) > 0 {
		if err := stored.write(records, make([][]byte, len(records))); err != nil {
			return 0, 0, err
		}
	}
	return changed, orphans, nil
}

type backfillOutcome int

const (
	backfillUnchanged backfillOutcome = iota
	backfillChanged
	backfillOrphan
)

func (stored *codeCollection) backfillRow(position int, backfill collection.ScalarBackfill) (rowRecord, backfillOutcome) {
	current := make(map[string]collection.ScalarValue, len(backfill.Columns))
	for _, column := range backfill.Columns {
		cell := stored.cell(position, column)
		value := cell.Value
		if cell.State != collection.ScalarCellValue {
			value = collection.ScalarValue{Type: column.Type, Null: true, String: "", Bool: false, Int64: 0}
		}
		current[column.Name] = value
	}
	unchanged := emptyRecord(recordUpsert, "")
	if !backfill.Needs(current) {
		return unchanged, backfillUnchanged
	}
	itemColumn := collection.ScalarColumn{Name: backfill.ItemColumn, Type: collection.ScalarTypeString, Nullable: true, MaxLength: 0}
	itemID := ""
	if cell := stored.cell(position, itemColumn); cell.State == collection.ScalarCellValue {
		itemID = cell.Value.String
	}
	values, streamed := backfill.ItemValues(itemID, stored.rows[position].relativePath)
	if !streamed {
		return unchanged, backfillOrphan
	}
	filled, changed := backfill.Filled(current, values)
	if !changed {
		return unchanged, backfillUnchanged
	}
	record := stored.recordOf(position)
	for columnName, value := range filled {
		column := stored.columnIndex[columnName]
		replaced := false
		for index := range record.cells {
			if record.cells[index].column == column {
				record.cells[index].value = value
				replaced = true
			}
		}
		if !replaced {
			record.cells = append(record.cells, storedCell{column: column, value: value})
		}
	}
	return record, backfillChanged
}

// Score returns one minus twice the fraction of differing bits.
func Score(distance int) float64 {
	return 1 - 2*float64(distance)/float64(staticembed.Dimensions)
}

// Search filters rows before ranking their sign codes by Hamming distance.
// It applies score, group and result limits to the best [collection.RankingDepth]
// candidates. Row IDs break ties between equal distances.
func (store *Store) Search(ctx context.Context, request collection.SearchRequest) ([]collection.Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, failed("search "+request.Collection, err)
	}
	if len(request.Vector) != staticembed.Dimensions {
		return nil, fmt.Errorf("search %s: query has %d dimensions, want %d", request.Collection, len(request.Vector), staticembed.Dimensions)
	}
	stored, err := store.collection(request.Collection)
	if err != nil {
		return nil, err
	}
	stored.mutex.RLock()
	defer stored.mutex.RUnlock()
	matches, err := stored.compileFilter(request.Filter)
	if err != nil {
		return nil, err
	}
	ranked, err := stored.rank(staticembed.CodeOf(request.Vector), matches)
	if err != nil {
		return nil, err
	}
	groupColumn, grouped := -1, false
	if request.GroupBy != "" && request.PerGroupLimit > 0 {
		groupColumn, grouped = stored.columnIndex[request.GroupBy]
	}
	limit := int(request.Limit)
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	perGroup := make(map[uint32]int32)
	hits := make([]collection.Hit, 0, limit)
	for _, ranking := range ranked {
		score := Score(ranking.distance)
		if score < request.MinScore {
			break
		}
		if grouped {
			key := stored.rows[ranking.position].cells[groupColumn]
			if key != 0 && stored.dictionaries[groupColumn].values[key].Null {
				key = 0
			}
			if perGroup[key] >= request.PerGroupLimit {
				continue
			}
			perGroup[key]++
		}
		hit, err := stored.hit(ranking.position, score, request.Declaration.Scalars)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit)
		if len(hits) == limit {
			break
		}
	}
	return hits, nil
}
