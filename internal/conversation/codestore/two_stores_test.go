package codestore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
						t.Fatalf("codestore.Open could not open the store: %v.", err)
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
		t.Fatalf("codestore.Open could not open the store: %v.", err)
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
		t.Fatalf("codestore.Open could not open the store: %v.", err)
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
		t.Fatalf("codestore.Open could not open the store: %v.", err)
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
				t.Fatalf("The test could not remove the data file: %v.", err)
			}
		},
		"only generation 3 files exist": func(t *testing.T, directory string) {
			t.Helper()
			for _, prefix := range []string{"rows", "data"} {
				if err := os.Rename(filepath.Join(directory, prefix+".log"), filepath.Join(directory, prefix+".3.log")); err != nil {
					t.Fatalf("The test could not rename the generation file: %v.", err)
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
				t.Fatalf("codestore.Open could not open the store: %v.", err)
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
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("The store returned rows %v; the test expected %v.", got, want)
	}
}
