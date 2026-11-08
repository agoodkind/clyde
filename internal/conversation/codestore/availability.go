package codestore

import (
	"errors"

	"goodkind.io/lm-semantic-search/collection"
)

// CollectionAvailable reads the collection header without creating files.
//
// A missing collection returns false. A closed store or an unreadable header
// returns an error.
//
//	available, err := store.CollectionAvailable(name)
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
