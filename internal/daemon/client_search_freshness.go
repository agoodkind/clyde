package daemon

import (
	"context"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

// GetSearchFreshness asks the daemon for the conversation feeder's latest sync
// snapshot, the same freshness each search response carries.
func GetSearchFreshness(ctx context.Context) (conversation.SearchFreshness, error) {
	empty := conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0}
	client, err := connectDaemon(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.GetSearchFreshness(rpcCtx, &clydev1.GetSearchFreshnessRequest{})
	if err != nil {
		return empty, daemonRPCError(rpcCtx, "get search freshness", err)
	}
	return searchFreshnessFromProto(resp.GetFreshness()), nil
}
