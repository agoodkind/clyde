package codestore

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"goodkind.io/lm-semantic-search/collection"
)

// RowUpdate returns replacement metadata and scalar values for a stored row.
// Its boolean result selects whether UpdateRows writes the row.
type RowUpdate func(id string, metadata string, cells map[string]collection.ScalarCell) (string, map[string]collection.ScalarValue, bool, error)

// UpdateRows reads and updates selected rows while holding the collection write lock.
// It preserves content and stored fields that the update does not replace.
func (store *Store) UpdateRows(ctx context.Context, name string, ids []string, columns []collection.ScalarColumn, update RowUpdate) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, failed("update rows of "+name, err)
	}
	stored, err := store.collection(name)
	if err != nil {
		return 0, err
	}
	stored.mutex.Lock()
	defer stored.mutex.Unlock()
	records := make([]rowRecord, 0, len(ids))
	blobs := make([][]byte, 0, len(ids))
	for _, id := range ids {
		position, found := stored.byID[id]
		if !found {
			continue
		}
		content, metadata, readErr := stored.readBlob(position)
		if readErr != nil {
			return 0, readErr
		}
		nextMetadata, scalars, apply, updateErr := update(id, metadata, stored.cells(position, columns))
		if updateErr != nil {
			return 0, updateErr
		}
		if !apply {
			continue
		}
		record := stored.recordOf(position)
		for columnName, value := range scalars {
			column, declared := stored.columnIndex[columnName]
			if !declared {
				return 0, fmt.Errorf("update row in collection with undeclared scalar column %s %q", name, columnName)
			}
			value.Type = stored.header.Declaration.Scalars[column].Type
			if value.Null {
				value = collection.ScalarValue{Type: value.Type, Null: true, String: "", Bool: false, Int64: 0}
			}
			record.cells = slices.DeleteFunc(record.cells, func(cell storedCell) bool { return cell.column == column })
			record.cells = append(record.cells, storedCell{column: column, value: value})
		}
		slices.SortFunc(record.cells, func(left, right storedCell) int { return cmp.Compare(left.column, right.column) })
		records = append(records, record)
		blobs = append(blobs, encodeBlob(content, nextMetadata))
	}
	if len(records) == 0 {
		return 0, nil
	}
	if err := stored.write(records, blobs); err != nil {
		return 0, err
	}
	return len(records), nil
}
