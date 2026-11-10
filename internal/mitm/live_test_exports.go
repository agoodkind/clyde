//go:build live

package mitm

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog"
)

// LiveIdentityCaptureProxy is an in-process MITM proxy wired for live
// identity-capture validation in isolated sandbox roots.
type LiveIdentityCaptureProxy struct {
	proxy *Proxy
	store *capture.Store
}

// NewLiveIdentityCaptureProxy opens a capture store and proxy that record
// exchanges under captureDir without binding a listener.
func NewLiveIdentityCaptureProxy(captureDir string, dbPath string, upstreamClient *http.Client) (*LiveIdentityCaptureProxy, error) {
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return nil, fmt.Errorf("open capture store %q: %w", dbPath, err)
	}
	path := filepath.Join(captureDir, slogger.ConcernRelPath(slogger.ConcernProviderMITMWire))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		_ = store.Close(context.Background(), "live identity proxy setup failed")
		return nil, fmt.Errorf("create wire log dir %q: %w", filepath.Dir(path), err)
	}
	logger := slog.New(gklog.FileJSON(path, slog.LevelDebug, gklog.RotationConfig{}))
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	tunnels := livetrack.Attach[TunnelMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseIngress,
		QuietRelevant: true,
		CancelNoWait:  false,
	}, livetrack.Options[TunnelMeta]{
		Component:     "mitm",
		Concern:       "providers.mitm.lifecycle",
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollEvery:     5 * time.Millisecond,
		CloserGrace:   200 * time.Millisecond,
		ParallelClose: false,
		Now:           nil,
	})
	proxy := &Proxy{
		log:             logger,
		httpClient:      upstreamClient,
		dialContext:     (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		store:           store,
		client:          "live-sandbox",
		Tunnels:         tunnels,
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir},
		base:            "http://[::1]",
		server:          nil,
	}
	return &LiveIdentityCaptureProxy{proxy: proxy, store: store}, nil
}

// Handle forwards one plain-HTTP exchange through the proxy capture path.
func (live *LiveIdentityCaptureProxy) Handle(w http.ResponseWriter, r *http.Request) {
	live.proxy.handle(w, r)
}

// Store returns the capture store backing the proxy.
func (live *LiveIdentityCaptureProxy) Store() *capture.Store {
	return live.store
}

// WaitForRequestRow blocks until requestID is committed to dbPath.
func WaitForRequestRow(dbPath string, requestID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		found, err := requestRowExists(dbPath, requestID)
		if err != nil {
			return err
		}
		if found {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

func requestRowExists(dbPath string, requestID string) (bool, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM requests WHERE request_id=?`, requestID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
