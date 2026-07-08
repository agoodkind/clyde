# Context Overview

Clyde serves a `ConversationContext` gRPC service that returns the last few turns of the newest conversation for a workspace. The service is defined in `api/context/v1/context.proto` and implemented in `internal/contextsvc/`.

## Surface

`ConversationContext.GetRecentTurns(workspace_ref, session_ref, turn_budget, max_chars_per_turn)` returns recent user and assistant turns, newest last, each truncated. The service registers on the daemon gRPC listener alongside the existing `clyde/v1` service in `internal/daemon/`.

## Resolution

`internal/contextsvc/service.go` resolves `workspace_ref` to the newest session through the existing conversation index (`conversation.Index.ListPage`) and loads that one conversation's messages (`LoadMessages`). It does not re-parse transcript files at call time. An empty result on no match is a normal reply, not an error.

## Address

An external client dials the address in `[daemon] grpc_address` (`internal/config/daemon_config.go`), which defaults to a `unix://` socket path. See `README.md`.

## Consumption

agent-gate calls `GetRecentTurns` to attach recent conversation context to a gated command before an LM judges it.

## Tests

`internal/contextsvc/service_test.go` and `internal/daemon/context_service_test.go` hold the behavior.
