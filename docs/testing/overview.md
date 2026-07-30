# Live daemon testing

The live harness validates a change by booting the real daemon in an isolated
sandbox and driving it, so the shipping binary is exercised while your running
daemon stays untouched.

## Overview

The suite lives under `test/live/` behind the `live` build tag, so an ordinary test
run never starts a daemon. Each test boots a throwaway daemon, drives its adapter or
MITM listeners, and shuts it down. Every test confirms the production daemon survived
unchanged.

## Run the suite

    make live

The suite is opt-in and runs only when you ask for it.

## Isolation

Each test gets its own state, config, and runtime directories under temp roots,
reached through the XDG environment variables. The daemon's socket, capture database,
and logs stay inside the sandbox, and it binds throwaway ports instead of the
production defaults. The production binary and its daemon are never touched.

## Parallel runs

Several isolated instances run at once, one per worktree or terminal. Each run reads
its ports from environment variables that fall back to the throwaway defaults, so give
concurrent runs distinct values to keep them apart.

| Variable | Default | Listener |
| --- | --- | --- |
| `CLYDE_TEST_ADAPTER_PORT` | 21434 | adapter HTTP |
| `CLYDE_TEST_MITM_PORT` | 58723 | MITM proxy |
| `CLYDE_TEST_CURSOR_PORT` | 21435 | cursor ingress |
| `CLYDE_TEST_TOPOLOGY_PORT` | 21436 | moved adapter port |
| `CLYDE_TEST_MOVED_MITM_PORT` | MITM port plus one | moved MITM port |
| `CLYDE_TEST_CONVERSATION_SEMANTIC` | false | conversation semantic enabled and search_enabled |
| `CLYDE_TEST_COLLECTION_ID` | fresh random id per run | conversation semantic collection_id |

A second run sets a disjoint set before it starts:

    CLYDE_TEST_ADAPTER_PORT=22434 CLYDE_TEST_MITM_PORT=59723 \
    CLYDE_TEST_CURSOR_PORT=22435 CLYDE_TEST_TOPOLOGY_PORT=22436 \
    CLYDE_TEST_MOVED_MITM_PORT=59724 CLYDE_TEST_CONVERSATION_SEMANTIC=true \
    CLYDE_TEST_COLLECTION_ID=clyde-live-run-2 make live

## The production-untouched invariant

Each test records the running production daemons before it boots and confirms they
survive the run. It counts only the installed daemon and skips test daemons under temp
paths, so a parallel run cannot trip it by mistake. A failure means the harness
touched a daemon it should not have.

## Preflight

The harness refuses to boot when a port it needs is already in use, when two of its
ports match, or when a sandbox directory sits outside a temp root. Refusing early stops
a run before it collides with another listener or hides a misconfiguration.

## Responses endpoint tests

The live harness runs the adapter and MITM capture store together. It sends
non-streaming and streaming `/v1/responses` requests through the real daemon to a
local passthrough upstream, then polls the isolated capture database for matching
adapter ingress and passthrough egress rows.

The [live Responses tests](../../test/live/responses_live_test.go) verify transparent
request fields, response bodies and headers, incremental lifecycle frames, and the
absence of the Chat `[DONE]` sentinel. They also keep a Responses stream active while
a reload-routed config edit waits for quiet, then confirm the reload proceeds after
the stream completes.

Ordinary test runs still cover the handler and compatibility contracts through the
[streaming writer tests](../../internal/adapter/responses_writer_test.go),
[response object tests](../../internal/adapter/openai/responses_response_test.go),
[compat warning surfaces test](../../internal/adapter/server_responses_compat_test.go),
and [compat catalog tests](../../internal/adapter/compat/compat_test.go).
