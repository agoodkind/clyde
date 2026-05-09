package claude

import (
	"context"
	"fmt"
	"os"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemonclient"
)

type daemonSessionClient interface {
	AcquireSession(wrapperID, sessionName, sessionID string) (*clydev1.AcquireSessionResponse, error)
	ReleaseSession(wrapperID string) error
	Close() error
}

type daemonSessionConnector func(context.Context) (daemonSessionClient, error)

var connectDaemonSession daemonSessionConnector = func(ctx context.Context) (daemonSessionClient, error) {
	client, err := daemon.ConnectOrStart(ctx)
	if err != nil {
		return nil, err
	}
	return daemonclient.New(client.Connection()), nil
}

func acquireDaemonSession(ctx context.Context, wrapperID, sessionName, sessionID string) string {
	client, err := connectDaemonSession(ctx)
	if err != nil {
		if VerboseFunc() {
			fmt.Fprintf(os.Stderr, "[DEBUG] daemon not available: %v\n", err)
		}
		return ""
	}
	defer func() { _ = client.Close() }()

	resp, acqErr := client.AcquireSession(wrapperID, sessionName, sessionID)
	if acqErr != nil {
		if VerboseFunc() {
			fmt.Fprintf(os.Stderr, "[DEBUG] daemon acquire failed: %v\n", acqErr)
		}
		return ""
	}
	return resp.GetSettingsFile()
}

// monitorDaemon runs alongside claude, periodically checking the daemon
// connection. If the daemon restarted (kill + relaunch during deploy),
// this re-acquires the session so global settings sync keeps working.
// On done signal, releases the session from whichever daemon is current.
func monitorDaemon(
	ctx context.Context,
	wrapperID, sessionName, sessionID string,
	done <-chan struct{},
	state *monitorState,
	stopped chan<- struct{},
) {
	const interval = 30 * time.Second
	ticker := time.NewTicker(interval)
	defer func() { ticker.Stop() }()
	defer close(stopped)

	for {
		select {
		case <-done:
			// The session ended, so release it from the current daemon.
			c, err := connectDaemonSession(ctx)
			if err == nil {
				_ = c.ReleaseSession(wrapperID)
				_ = c.Close()
			}
			return

		case <-ticker.C:
			// Health check: try to connect and re-acquire.
			// ConnectOrStart is idempotent (flock prevents double-start).
			// AcquireSession is idempotent (daemon overwrites existing entry).
			c, err := connectDaemonSession(ctx)
			if err != nil {
				state.sawConnectionError = true
				if VerboseFunc() {
					fmt.Fprintf(os.Stderr, "[DEBUG] daemon monitor: connect failed: %v\n", err)
				}
				continue
			}
			_, acqErr := c.AcquireSession(wrapperID, sessionName, sessionID)
			_ = c.Close()
			if acqErr != nil {
				state.sawConnectionError = true
			}
			if acqErr != nil && VerboseFunc() {
				fmt.Fprintf(os.Stderr, "[DEBUG] daemon monitor: re-acquire failed: %v\n", acqErr)
			}
			if acqErr == nil && state.sawConnectionError {
				state.reloadRequested.Store(true)
				state.sawConnectionError = false
				claudeLifecycleLog.Logger().Debug("wrapper.self_reload.requested",
					"component", "wrapper",
					"session", sessionName,
					"session_id", sessionID,
					"wrapper_id", wrapperID,
					"reason", "daemon_reconnected")
			}
		}
	}
}
