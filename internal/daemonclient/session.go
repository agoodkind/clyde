// Package daemonclient exposes narrow Clyde daemon RPC clients for packages
// that must not import the full daemon server package.
package daemonclient

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/gklog"
)

// Client exposes session-isolation RPCs used by provider wrappers.
type Client interface {
	AcquireSession(wrapperID, sessionName, sessionID string) (*clydev1.AcquireSessionResponse, error)
	ReleaseSession(wrapperID string) error
	CreateSession(ctx context.Context, sessionName, basedir, provider string, incognito bool) (*clydev1.CreateSessionResponse, error)
	Close() error
}

// GRPCClient implements Client over an existing daemon gRPC connection.
type GRPCClient struct {
	conn *grpc.ClientConn
	rpc  clydev1.ClydeServiceClient
}

// New wraps an existing gRPC connection with session-isolation helpers.
func New(conn *grpc.ClientConn) Client {
	return &GRPCClient{
		conn: conn,
		rpc:  clydev1.NewClydeServiceClient(conn),
	}
}

func (c *GRPCClient) AcquireSession(wrapperID, sessionName, sessionID string) (*clydev1.AcquireSessionResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := clientLog(ctx)
	log.DebugContext(ctx, "daemon.client.acquire_session.begin",
		"wrapper_id", wrapperID,
		"session_name", sessionName,
		"session_id", sessionID,
	)
	return c.rpc.AcquireSession(ctx, &clydev1.AcquireSessionRequest{
		WrapperId:   wrapperID,
		SessionName: sessionName,
		SessionId:   sessionID,
	})
}

func (c *GRPCClient) ReleaseSession(wrapperID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := clientLog(ctx)
	log.DebugContext(ctx, "daemon.client.release_session.begin", "wrapper_id", wrapperID)
	_, err := c.rpc.ReleaseSession(ctx, &clydev1.ReleaseSessionRequest{
		WrapperId: wrapperID,
	})
	return err
}

// CreateSession asks the daemon to mint a provider session id and persist the
// session record, returning the name and id. The CLI calls this so the daemon
// owns record creation, then launches the tool with the returned id.
func (c *GRPCClient) CreateSession(ctx context.Context, sessionName, basedir, provider string, incognito bool) (*clydev1.CreateSessionResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	log := clientLog(callCtx)
	log.DebugContext(callCtx, "daemon.client.create_session.begin",
		"session_name", sessionName,
		"basedir", basedir,
		"provider", provider,
	)
	resp, err := c.rpc.CreateSession(callCtx, &clydev1.CreateSessionRequest{
		SessionName: sessionName,
		Basedir:     basedir,
		Incognito:   incognito,
		Provider:    provider,
	})
	if err != nil {
		log.ErrorContext(callCtx, "daemon.client.create_session.failed",
			"session_name", sessionName,
			"err", err,
		)
		return nil, fmt.Errorf("create session rpc: %w", err)
	}
	return resp, nil
}

func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

func clientLog(ctx context.Context) *slog.Logger {
	if ctx == nil {
		ctx = context.Background()
	}
	return gklog.LoggerFromContext(ctx).With("component", "daemon", "subcomponent", "client")
}
