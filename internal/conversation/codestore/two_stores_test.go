package codestore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	twoStoresCompactRows = 10002
	readWaitObservation  = 300 * time.Millisecond
	lockTestResumeLimit  = 30 * time.Second
)

// TestTwoStoresKeepEachOthersRows alternates writes between two stores on one root.
// Both stores and a newly opened third store read every row with the expected content.
func TestTwoStoresKeepEachOthersRows(t *testing.T) {
	root := t.TempDir()
	first, model := openCollection(t, root)
	defer first.Close()
	upsert(t, first, testRow(t, model, "first-a", "claude:first", "alpha row written by the first store"))

	second, _ := openCollection(t, root)
	defer second.Close()
	upsert(t, second, testRow(t, model, "second-b", "claude:second", "bravo row written by the second store"))
	upsert(t, first, testRow(t, model, "first-c", "claude:first", "gamma row written by the first store"))
	upsert(t, second, testRow(t, model, "second-d", "claude:second", "delta row written by the second store"))

	want := map[string]string{
		"first-a":  "alpha row written by the first store",
		"second-b": "bravo row written by the second store",
		"first-c":  "gamma row written by the first store",
		"second-d": "delta row written by the second store",
	}
	requireStoreContents(t, first, want)
	requireStoreContents(t, second, want)
	reopened, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("codestore.Open could not open the third store: %v.", err)
	}
	defer reopened.Close()
	requireStoreContents(t, reopened, want)
}

// TestStoreReadsAndWritesAfterAnotherStoreCompacts compacts one store's generation.
// The other store reads the new generation and writes a row that both stores read.
func TestStoreReadsAndWritesAfterAnotherStoreCompacts(t *testing.T) {
	root := t.TempDir()
	first, model := openCollection(t, root)
	defer first.Close()
	upsert(t, first, testRow(t, model, "kept", "claude:first", "row before compaction"))
	second, _ := openCollection(t, root)
	defer second.Close()
	requireStoreContents(t, second, map[string]string{"kept": "row before compaction"})

	repeated := make([]collection.Row, 0, twoStoresCompactRows)
	row := testRow(t, model, "rewritten", "claude:first", "row written many times")
	for range twoStoresCompactRows {
		repeated = append(repeated, row)
	}
	upsert(t, first, repeated...)

	want := map[string]string{"kept": "row before compaction", "rewritten": "row written many times"}
	requireStoreContents(t, second, want)
	upsert(t, second, testRow(t, model, "later", "claude:second", "row after compaction"))
	want["later"] = "row after compaction"
	requireStoreContents(t, first, want)
	requireStoreContents(t, second, want)
}

// TestStoreReadWaitsForAnotherStoreLock checks that Query waits for the test's
// exclusive kernel lock on the collection lock file. Query must return one row
// after the test releases the lock.
func TestStoreReadWaitsForAnotherStoreLock(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	defer store.Close()
	upsert(t, store, testRow(t, model, "kept", "claude:first", "row before the lock"))

	lock := takeCollectionLock(t, root)
	defer func() { _ = lock.Close() }()

	type readResult struct {
		hits []collection.Hit
		err  error
	}
	finished := make(chan readResult, 1)
	go func() {
		hits, queryErr := store.Query(context.Background(), collection.QueryRequest{Collection: testCollection, Filter: nil, Limit: 0, Declaration: declaration()})
		finished <- readResult{hits: hits, err: queryErr}
	}()
	select {
	case result := <-finished:
		t.Fatalf("Query finished while the test owned the exclusive kernel lock. It returned error %v.", result.err)
	case <-time.After(readWaitObservation):
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("The test could not release the exclusive kernel lock: %v.", err)
	}
	select {
	case result := <-finished:
		if result.err != nil {
			t.Fatalf("The read failed after the test released the exclusive kernel lock: %v.", result.err)
		}
		if len(result.hits) != 1 {
			t.Fatalf("The read returned %d rows instead of the expected one row.", len(result.hits))
		}
	case <-time.After(lockTestResumeLimit):
		t.Fatalf("The read did not finish within %v after the test released the exclusive kernel lock.", lockTestResumeLimit)
	}
}

// TestStoreLockWaitEndsWhenRequestCancels cancels Query and Upsert requests while
// the test owns the exclusive kernel lock. Each request must return an error that
// matches context.Canceled.
func TestStoreLockWaitEndsWhenRequestCancels(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	defer store.Close()
	row := testRow(t, model, "kept", "claude:first", "row before the lock")
	upsert(t, store, row)
	lock := takeCollectionLock(t, root)
	defer func() { _ = lock.Close() }()

	requests := map[string]func(context.Context) error{
		"read": func(ctx context.Context) error {
			_, err := store.Query(ctx, collection.QueryRequest{Collection: testCollection, Filter: nil, Limit: 0, Declaration: declaration()})
			return err
		},
		"write": func(ctx context.Context) error {
			return store.Upsert(ctx, testCollection, declaration(), []collection.Row{row})
		},
	}
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() { finished <- request(ctx) }()
			select {
			case err := <-finished:
				t.Fatalf("The request finished before the test canceled its context. It returned error %v.", err)
			case <-time.After(readWaitObservation):
			}
			cancel()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("The canceled request returned %v; the test expected an error matching context.Canceled.", err)
				}
			case <-time.After(lockTestResumeLimit):
				t.Fatalf("The request did not end within %v after cancellation.", lockTestResumeLimit)
			}
		})
	}
}

// TestStoreRejectsRequestsAfterClose checks that Load and EnsureCollection return
// ErrStoreClosed after Close without creating collection handles.
func TestStoreRejectsRequestsAfterClose(t *testing.T) {
	root := t.TempDir()
	store, _ := openCollection(t, root)
	store.Close()
	ctx := context.Background()
	if _, err := store.Load(ctx, testCollection); !errors.Is(err, codestore.ErrStoreClosed) {
		t.Fatalf("Load returned %v instead of ErrStoreClosed after Close.", err)
	}
	ensure := collection.EnsureRequest{Collection: testCollection, Declaration: declaration(), Dimension: staticembed.Dimensions}
	if err := store.EnsureCollection(ctx, ensure); !errors.Is(err, codestore.ErrStoreClosed) {
		t.Fatalf("EnsureCollection returned %v instead of ErrStoreClosed after Close.", err)
	}
}

// TestStoreCloseEndsRequestsWaitingForCollectionLock checks Query and Upsert
// requests that wait for an exclusive kernel lock. The test closes the store
// before releasing the lock. Each request must return ErrStoreClosed whether
// the store has already loaded the collection or has not loaded it.
func TestStoreCloseEndsRequestsWaitingForCollectionLock(t *testing.T) {
	requests := map[string]func(*codestore.Store, collection.Row) error{
		"read": func(store *codestore.Store, _ collection.Row) error {
			_, err := store.Query(context.Background(), collection.QueryRequest{Collection: testCollection, Filter: nil, Limit: 0, Declaration: declaration()})
			return err
		},
		"write": func(store *codestore.Store, row collection.Row) error {
			return store.Upsert(context.Background(), testCollection, declaration(), []collection.Row{row})
		},
	}
	for _, loaded := range []bool{true, false} {
		for name, request := range requests {
			t.Run(fmt.Sprintf("%s loaded=%t", name, loaded), func(t *testing.T) {
				root := t.TempDir()
				store, model := openCollection(t, root)
				row := testRow(t, model, "kept", "claude:first", "row before the lock")
				upsert(t, store, row)
				if !loaded {
					store.Close()
					reopened, err := codestore.Open(root, staticembed.ModelName)
					if err != nil {
						t.Fatalf("codestore.Open failed to open the store: %v.", err)
					}
					store = reopened
				}
				lock := takeCollectionLock(t, root)
				defer func() { _ = lock.Close() }()

				finished := make(chan error, 1)
				go func() { finished <- request(store, row) }()
				select {
				case err := <-finished:
					t.Fatalf("The request returned %v before the test released the exclusive kernel lock.", err)
				case <-time.After(readWaitObservation):
				}
				closed := make(chan struct{})
				go func() {
					store.Close()
					close(closed)
				}()
				select {
				case <-closed:
				case <-time.After(lockTestResumeLimit):
					t.Fatalf("Close did not finish within %v while the request waited for the exclusive kernel lock.", lockTestResumeLimit)
				}
				if err := lock.Close(); err != nil {
					t.Fatalf("The test could not release the exclusive kernel lock: %v.", err)
				}
				select {
				case err := <-finished:
					if !errors.Is(err, codestore.ErrStoreClosed) {
						t.Fatalf("The waiting request returned %v instead of ErrStoreClosed.", err)
					}
				case <-time.After(lockTestResumeLimit):
					t.Fatalf("The request did not end within %v after the test released the exclusive kernel lock.", lockTestResumeLimit)
				}
			})
		}
	}
}

// TestEnsureCollectionCreatesNothingAfterCancellation checks that EnsureCollection
// returns an error matching context.Canceled without creating the collection
// directory when the test cancels the request before the call or during the wait
// for the kernel lock.
func TestEnsureCollectionCreatesNothingAfterCancellation(t *testing.T) {
	root := t.TempDir()
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("codestore.Open failed to open the store: %v.", err)
	}
	defer store.Close()
	ensure := collection.EnsureRequest{Collection: testCollection, Declaration: declaration(), Dimension: staticembed.Dimensions}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := store.EnsureCollection(canceled, ensure); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureCollection returned %v; the test expected an error matching context.Canceled.", err)
	}
	requireNoCollection(t, root)

	lock := createCollectionLock(t, root)
	defer func() { _ = lock.Close() }()
	waiting, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	finished := make(chan error, 1)
	go func() { finished <- store.EnsureCollection(waiting, ensure) }()
	select {
	case err := <-finished:
		t.Fatalf("EnsureCollection returned %v before the test released the exclusive kernel lock.", err)
	case <-time.After(readWaitObservation):
	}
	cancelWait()
	if err := lock.Close(); err != nil {
		t.Fatalf("The test could not release the exclusive kernel lock: %v.", err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EnsureCollection returned %v; the test expected an error matching context.Canceled.", err)
		}
	case <-time.After(lockTestResumeLimit):
		t.Fatalf("EnsureCollection did not end within %v after cancellation.", lockTestResumeLimit)
	}
	requireNoCollection(t, root)
}

// TestEnsureCollectionCreatesNothingAfterClose starts EnsureCollection while the test
// owns the exclusive kernel lock, then calls Close before it releases the lock.
// It checks that EnsureCollection returns an error matching ErrStoreClosed and
// does not create the collection directory.
func TestEnsureCollectionCreatesNothingAfterClose(t *testing.T) {
	root := t.TempDir()
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("codestore.Open failed to open the store: %v.", err)
	}
	lock := createCollectionLock(t, root)
	defer func() { _ = lock.Close() }()
	ensure := collection.EnsureRequest{Collection: testCollection, Declaration: declaration(), Dimension: staticembed.Dimensions}
	finished := make(chan error, 1)
	go func() { finished <- store.EnsureCollection(context.Background(), ensure) }()
	select {
	case err := <-finished:
		t.Fatalf("EnsureCollection returned %v before the test released the exclusive kernel lock.", err)
	case <-time.After(readWaitObservation):
	}
	store.Close()
	if err := lock.Close(); err != nil {
		t.Fatalf("The test could not release the exclusive kernel lock: %v.", err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, codestore.ErrStoreClosed) {
			t.Fatalf("EnsureCollection returned %v; the test expected an error matching ErrStoreClosed.", err)
		}
	case <-time.After(lockTestResumeLimit):
		t.Fatalf("EnsureCollection did not end within %v after the test released the lock.", lockTestResumeLimit)
	}
	requireNoCollection(t, root)
}

func createCollectionLock(t *testing.T, root string) *os.File {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(root, testCollection+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("The test could not create the collection lock file: %v.", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("The test does not own the exclusive kernel lock: %v.", err)
	}
	return lock
}

func requireNoCollection(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, testCollection)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat returned %v; the test expected an error matching os.ErrNotExist.", err)
	}
}

// TestStoreFailsWhenCurrentGenerationFilesAreMissing changes the header to a generation
// without files. Load must return an error without creating or removing files.
func TestStoreFailsWhenCurrentGenerationFilesAreMissing(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	upsert(t, store, testRow(t, model, "kept", "claude:first", "row in the older generation"))
	store.Close()

	directory := filepath.Join(root, testCollection)
	headerPath := filepath.Join(directory, "collection.json")
	raw, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatalf("The test could not read the collection header: %v.", err)
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatalf("The test could not parse the collection header: %v.", err)
	}
	header["generation"] = json.RawMessage("7")
	changed, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("The test could not encode the collection header: %v.", err)
	}
	if err := os.WriteFile(headerPath, changed, 0o600); err != nil {
		t.Fatalf("The test could not write the collection header: %v.", err)
	}
	filesBefore := directoryNames(t, directory)

	reopened, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("codestore.Open failed to open the store: %v.", err)
	}
	defer reopened.Close()
	if _, err := reopened.Load(context.Background(), testCollection); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load returned %v instead of a missing file error.", err)
	}
	if filesAfter := directoryNames(t, directory); !slices.Equal(filesAfter, filesBefore) {
		t.Fatalf("After Load, the collection directory contains %v; the test expected %v.", filesAfter, filesBefore)
	}
}

// TestStoreKeepsFilesWhenGenerationZeroIsIncomplete removes or renames generation 0
// files. Load must return an error without creating or removing files.
func TestStoreKeepsFilesWhenGenerationZeroIsIncomplete(t *testing.T) {
	cases := map[string]func(t *testing.T, directory string){
		"the row log and data file do not exist": func(t *testing.T, directory string) {
			t.Helper()
			for _, name := range []string{"rows.log", "data.log"} {
				if err := os.Remove(filepath.Join(directory, name)); err != nil {
					t.Fatalf("The test could not remove the generation file: %v.", err)
				}
			}
		},
		"the data file does not exist": func(t *testing.T, directory string) {
			t.Helper()
			if err := os.Remove(filepath.Join(directory, "data.log")); err != nil {
				t.Fatalf("The test failed to remove the data file: %v.", err)
			}
		},
		"only generation 3 files exist": func(t *testing.T, directory string) {
			t.Helper()
			for _, prefix := range []string{"rows", "data"} {
				if err := os.Rename(filepath.Join(directory, prefix+".log"), filepath.Join(directory, prefix+".3.log")); err != nil {
					t.Fatalf("The test failed to rename the generation file: %v.", err)
				}
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store, model := openCollection(t, root)
			upsert(t, store, testRow(t, model, "kept", "claude:first", "row in generation zero"))
			store.Close()
			directory := filepath.Join(root, testCollection)
			damage(t, directory)
			filesBefore := directoryNames(t, directory)

			reopened, err := codestore.Open(root, staticembed.ModelName)
			if err != nil {
				t.Fatalf("codestore.Open failed to open the store: %v.", err)
			}
			defer reopened.Close()
			if _, err := reopened.Load(context.Background(), testCollection); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Load returned %v instead of a missing file error.", err)
			}
			if filesAfter := directoryNames(t, directory); !slices.Equal(filesAfter, filesBefore) {
				t.Fatalf("After Load, the collection directory contains %v; the test expected %v.", filesAfter, filesBefore)
			}
		})
	}
}

// TestEnsureCollectionKeepsGenerationFilesWithoutHeader removes the collection header
// after storing a row. It checks that Load and EnsureCollection return errors other
// than ErrCollectionMissing and preserve every remaining file.
func TestEnsureCollectionKeepsGenerationFilesWithoutHeader(t *testing.T) {
	cases := map[string]func(t *testing.T, directory string){
		"generation 0 files exist": func(_ *testing.T, _ string) {},
		"only generation 3 files exist": func(t *testing.T, directory string) {
			t.Helper()
			for _, prefix := range []string{"rows", "data"} {
				if err := os.Rename(filepath.Join(directory, prefix+".log"), filepath.Join(directory, prefix+".3.log")); err != nil {
					t.Fatalf("The test failed to rename the generation file: %v.", err)
				}
			}
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store, model := openCollection(t, root)
			upsert(t, store, testRow(t, model, "kept", "claude:first", "row before the header removal"))
			store.Close()
			directory := filepath.Join(root, testCollection)
			if err := os.Remove(filepath.Join(directory, "collection.json")); err != nil {
				t.Fatalf("The test failed to remove the collection header: %v.", err)
			}
			arrange(t, directory)
			filesBefore := directoryContents(t, directory)

			reopened, err := codestore.Open(root, staticembed.ModelName)
			if err != nil {
				t.Fatalf("codestore.Open failed to open the store: %v.", err)
			}
			defer reopened.Close()
			ctx := context.Background()
			if _, err := reopened.Load(ctx, testCollection); err == nil || errors.Is(err, collection.ErrCollectionMissing) {
				t.Errorf("Load returned %v instead of an error that does not match ErrCollectionMissing.", err)
			}
			ensure := collection.EnsureRequest{Collection: testCollection, Declaration: declaration(), Dimension: staticembed.Dimensions}
			if err := reopened.EnsureCollection(ctx, ensure); err == nil || errors.Is(err, collection.ErrCollectionMissing) {
				t.Errorf("EnsureCollection returned %v instead of an error that does not match ErrCollectionMissing.", err)
			}
			if !maps.EqualFunc(directoryContents(t, directory), filesBefore, bytes.Equal) {
				t.Fatalf("EnsureCollection changed a file name or file contents in the collection directory.")
			}
		})
	}
}

// TestEnsureCollectionFinishesInterruptedCreation creates empty generation 0 files
// without a header. It checks that EnsureCollection creates the header and that
// the collection accepts and returns a row.
func TestEnsureCollectionFinishesInterruptedCreation(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, testCollection)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("The test failed to create the collection directory: %v.", err)
	}
	for _, name := range []string{"rows.log", "data.log"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o600); err != nil {
			t.Fatalf("The test failed to create the generation file: %v.", err)
		}
	}
	store, model := openCollection(t, root)
	defer store.Close()
	upsert(t, store, testRow(t, model, "kept", "claude:first", "row after the interrupted creation"))
	requireStoreContents(t, store, map[string]string{"kept": "row after the interrupted creation"})
}

// TestStoreRejectsWriteAfterDataFileIsRemoved deletes the data file while the store
// has it open. It checks that Upsert returns an error matching os.ErrNotExist
// and does not change the row log size.
func TestStoreRejectsWriteAfterDataFileIsRemoved(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	defer store.Close()
	upsert(t, store, testRow(t, model, "kept", "claude:first", "row before the removal"))
	directory := filepath.Join(root, testCollection)
	if err := os.Remove(filepath.Join(directory, "data.log")); err != nil {
		t.Fatalf("The test failed to remove the data file: %v.", err)
	}
	rowLogSize := fileSize(t, rowLogPath(root))

	row := testRow(t, model, "later", "claude:first", "row after the removal")
	err := store.Upsert(context.Background(), testCollection, declaration(), []collection.Row{row})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Upsert returned %v instead of an error matching os.ErrNotExist.", err)
	}
	if got := fileSize(t, rowLogPath(root)); got != rowLogSize {
		t.Fatalf("The row log contains %d bytes after Upsert failed; the test expected %d bytes.", got, rowLogSize)
	}
}

// TestStoreWritesToReplacementDataFile replaces the data file with a copy while
// the store has the original open. It checks that a new store reads the original
// row and a row that the first store writes after the replacement.
func TestStoreWritesToReplacementDataFile(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	defer store.Close()
	upsert(t, store, testRow(t, model, "kept", "claude:first", "row before the replacement"))
	dataPath := filepath.Join(root, testCollection, "data.log")
	content, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("The test failed to read the data file: %v.", err)
	}
	replacementPath := dataPath + ".replacement"
	if err := os.WriteFile(replacementPath, content, 0o600); err != nil {
		t.Fatalf("The test failed to write the replacement data file: %v.", err)
	}
	if err := os.Rename(replacementPath, dataPath); err != nil {
		t.Fatalf("The test failed to replace the original data file with the copy: %v.", err)
	}

	upsert(t, store, testRow(t, model, "later", "claude:first", "row after the replacement"))
	reopened, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("codestore.Open failed to open the store: %v.", err)
	}
	defer reopened.Close()
	requireStoreContents(t, reopened, map[string]string{"kept": "row before the replacement", "later": "row after the replacement"})
}

// TestStoreKeepsRowLogWhenDataFileIsShorter shortens the data file by one byte.
// The test checks that Query, Upsert, and Load return errors and preserve every
// file when the last row references bytes beyond the end of the file. After the
// test restores the original data file, a new store must read every original row.
func TestStoreKeepsRowLogWhenDataFileIsShorter(t *testing.T) {
	cases := map[string]func(t *testing.T, path string, shorter []byte){
		"another file replaces the data file": func(t *testing.T, path string, shorter []byte) {
			t.Helper()
			replacement := path + ".replacement"
			if err := os.WriteFile(replacement, shorter, 0o600); err != nil {
				t.Fatalf("os.WriteFile failed to write the shorter data file: %v.", err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatalf("os.Rename failed to replace the data file: %v.", err)
			}
		},
		"the data file becomes shorter in place": func(t *testing.T, path string, shorter []byte) {
			t.Helper()
			if err := os.Truncate(path, int64(len(shorter))); err != nil {
				t.Fatalf("os.Truncate failed to shorten the data file: %v.", err)
			}
		},
	}
	for name, shorten := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store, model := openCollection(t, root)
			defer store.Close()
			want := map[string]string{"kept": "row before the shorter file", "last": "last row before the shorter file"}
			upsert(t, store, testRow(t, model, "kept", "claude:first", want["kept"]))
			upsert(t, store, testRow(t, model, "last", "claude:first", want["last"]))
			directory := filepath.Join(root, testCollection)
			dataPath := filepath.Join(directory, "data.log")
			original, err := os.ReadFile(dataPath)
			if err != nil {
				t.Fatalf("os.ReadFile failed to read the data file: %v.", err)
			}
			shorten(t, dataPath, original[:len(original)-1])
			filesBefore := directoryContents(t, directory)

			ctx := context.Background()
			query := collection.QueryRequest{Collection: testCollection, Declaration: declaration(), Filter: nil, Limit: 0}
			if _, err := store.Query(ctx, query); err == nil {
				t.Errorf("Query did not return an error after the test shortened the data file.")
			}
			later := testRow(t, model, "later", "claude:first", "row after the shorter file")
			if err := store.Upsert(ctx, testCollection, declaration(), []collection.Row{later}); err == nil {
				t.Errorf("Upsert did not return an error after the test shortened the data file.")
			}
			reopened, err := codestore.Open(root, staticembed.ModelName)
			if err != nil {
				t.Fatalf("codestore.Open failed to open the store: %v.", err)
			}
			defer reopened.Close()
			if _, err := reopened.Load(ctx, testCollection); err == nil {
				t.Errorf("Load did not return an error after the test shortened the data file.")
			}
			if !maps.EqualFunc(directoryContents(t, directory), filesBefore, bytes.Equal) {
				t.Fatalf("A request changed a file name or file contents in the collection directory.")
			}

			if err := os.WriteFile(dataPath, original, 0o600); err != nil {
				t.Fatalf("os.WriteFile failed to restore the data file: %v.", err)
			}
			restored, err := codestore.Open(root, staticembed.ModelName)
			if err != nil {
				t.Fatalf("codestore.Open failed to open the store: %v.", err)
			}
			defer restored.Close()
			requireStoreContents(t, restored, want)
		})
	}
}

func directoryContents(t *testing.T, directory string) map[string][]byte {
	t.Helper()
	contents := make(map[string][]byte)
	for _, name := range directoryNames(t, directory) {
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatalf("The test failed to read a collection file: %v.", err)
		}
		contents[name] = content
	}
	return contents
}

func takeCollectionLock(t *testing.T, root string) *os.File {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(root, testCollection+".lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("The test could not open the collection lock file: %v.", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("The test could not take the exclusive kernel lock: %v.", err)
	}
	return lock
}

func directoryNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("The test could not list the collection directory: %v.", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func requireStoreContents(t *testing.T, store *codestore.Store, want map[string]string) {
	t.Helper()
	got := contents(t, store)
	if !maps.Equal(got, want) {
		t.Fatalf("The store returned rows %v; the test expected %v.", got, want)
	}
}
