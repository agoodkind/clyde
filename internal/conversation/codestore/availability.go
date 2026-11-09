package codestore

import (
	"errors"

	"goodkind.io/lm-semantic-search/collection"
)

// CollectionAvailable reads the collection header without creating files.
//
// A missing collection is an ordinary unavailable result. A closed store,
// invalid name, or unreadable header returns an error instead.
//
// The results distinguish these cases:
//   - A readable header returns true and a nil error.
//   - A missing collection returns false and a nil error.
//   - A failed check returns false and the error.
func (store *Store) CollectionAvailable(name string) (bool, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if store.closed {
		return false, ErrStoreClosed
	}
	directory, err := store.directory(name)
	if err != nil {
		return false, err
	}
	_, _, err = readHeader(directory)
	if errors.Is(err, collection.ErrCollectionMissing) {
		return false, nil
	}
	if err != nil {
		return false, failed("check the collection header in "+directory, err)
	}
	return true, nil
}
