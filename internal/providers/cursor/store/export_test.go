package cursorstore

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
)

// DiscoveryReadCounts measures actual driver connections and SELECT authorizations.
type DiscoveryReadCounts struct{ Opens, SelectAuthorizations int }

// DiscoveryReadCounter keeps measurements isolated by database filename.
type DiscoveryReadCounter struct {
	mu          sync.Mutex
	counts      map[string]DiscoveryReadCounts
	attempts    map[string]int
	pausePath   string
	readEntered chan bool
	readRelease chan bool
}

var testDriverSequence atomic.Uint64

// ObserveDiscoveryReads replaces only Clyde's readonly driver for this test.
func ObserveDiscoveryReads(t *testing.T) *DiscoveryReadCounter {
	t.Helper()
	counter := &DiscoveryReadCounter{counts: make(map[string]DiscoveryReadCounts), attempts: make(map[string]int)}
	name := fmt.Sprintf("cursor-discovery-test-%d", testDriverSequence.Add(1))
	sql.Register(name, &observedSQLiteDriver{counter: counter, driver: &sqlite3.SQLiteDriver{ConnectHook: func(connection *sqlite3.SQLiteConn) error {
		path := connection.GetFilename("main")
		counter.mu.Lock()
		value := counter.counts[path]
		value.Opens++
		counter.counts[path] = value
		counter.mu.Unlock()
		connection.RegisterAuthorizer(func(operation int, first, second, database string) int {
			if operation == sqlite3.SQLITE_SELECT {
				counter.mu.Lock()
				pause := path == counter.pausePath
				entered, release := counter.readEntered, counter.readRelease
				if pause {
					counter.pausePath = ""
				}
				value := counter.counts[path]
				value.SelectAuthorizations++
				counter.counts[path] = value
				counter.mu.Unlock()
				if pause {
					entered <- true
					<-release
				}
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}}})
	previous := readOnlyDriverName
	readOnlyDriverName = name
	t.Cleanup(func() { readOnlyDriverName = previous })
	return counter
}

type observedSQLiteDriver struct {
	driver  *sqlite3.SQLiteDriver
	counter *DiscoveryReadCounter
}

func (observed *observedSQLiteDriver) Open(name string) (driver.Conn, error) {
	parsed, err := url.Parse(name)
	if err != nil {
		return nil, err
	}
	path := parsed.Path
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}
	observed.counter.mu.Lock()
	observed.counter.attempts[path]++
	observed.counter.mu.Unlock()
	return observed.driver.Open(name)
}

// TakeOpenAttempts includes driver opens that failed before ConnectHook ran.
func (counter *DiscoveryReadCounter) TakeOpenAttempts(path string) int {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	count := counter.attempts[path]
	delete(counter.attempts, path)
	return count
}

// PauseNextSelect holds one actual SQLite read while unrelated consumers run.
func (counter *DiscoveryReadCounter) PauseNextSelect(path string) (<-chan bool, chan<- bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	counter.pausePath = path
	counter.readEntered = make(chan bool, 1)
	counter.readRelease = make(chan bool, 1)
	return counter.readEntered, counter.readRelease
}

// Take returns and clears the observed counts for one file.
func (counter *DiscoveryReadCounter) Take(path string) DiscoveryReadCounts {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	value := counter.counts[path]
	delete(counter.counts, path)
	return value
}

// CreateDiscoveryFixture reuses the global and workspace fixture builders.
func CreateDiscoveryFixture(t *testing.T) (DataRoot, WorkspaceEntry) {
	t.Helper()
	rootDir := t.TempDir()
	root := DataRoot{RootDir: rootDir, GlobalDBPath: filepath.Join(rootDir, "globalStorage", "state.vscdb"), WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage")}
	if err := os.MkdirAll(filepath.Dir(root.GlobalDBPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(createCursorStoreTestDatabase(t), root.GlobalDBPath); err != nil {
		t.Fatal(err)
	}
	writeSharedComposerWorkspace(t, root, "workspace", `{"folder":"file:///tmp/project"}`)
	entry := WorkspaceEntry{WorkspaceHash: "workspace", StateDBPath: filepath.Join(root.WorkspaceStorageDir, "workspace", "state.vscdb"), WorkspaceJSONPath: filepath.Join(root.WorkspaceStorageDir, "workspace", "workspace.json")}
	return root, entry
}
