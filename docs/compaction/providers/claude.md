# Claude provider: compaction internals

The Claude provider implements the two probes the compaction planner depends on. The first is the live /context probe that measures the actual per-session overhead. The second is the CandidateProber that measures projection for a synthesized post-boundary candidate transcript. Both probes live in `internal/providers/claude/contextusage/` because they carry Claude-specific spawn shape, control-request envelopes, and /context output parsing, and the generic `internal/contextusage/` package stays provider-neutral.

See [../algorithm.md](../algorithm.md) for how the planner consumes these probes and [../edge-cases.md](../edge-cases.md) for the probe failure modes.

## The `/context` probe shape

`ProbeContextUsage` in `probe_spawn.go` spawns the `claude` binary in SDK stream-json mode against a target session and asks it for its current `/context` snapshot. The spawn command attaches stdin, stdout, and stderr pipes, sends one JSON-encoded control request on stdin, scans stdout for the matching control response, and exits after the response arrives. Stderr is drained concurrently so a noisy claude does not block on a full pipe buffer; the first 2048 bytes are kept as a tail for failure diagnostics.

The command line carries the flags every probe spawn needs. The `-p` flag puts claude in print-mode rather than the interactive REPL. The `--resume <session-id>` flag resumes the target session so the snapshot includes that session's actual model choice, custom system prompt, injected context, and tool surface. The `--input-format stream-json` and `--output-format stream-json` flags switch claude to the SDK protocol the probe needs. The `--verbose` flag is required by claude when stream-json is in use. The `--no-session-persistence` flag is the rule from [../canary-system.md](../canary-system.md): the probe reads the transcript and writes nothing back to disk.

The probe closes stdin as soon as it has written its control request. Keeping stdin open would leave claude waiting for additional messages until the spawn context deadline expires, so the explicit close is what lets claude see EOF and shut down cleanly after emitting its response.

## The `control_request` and `control_response` envelopes

The probe communicates with claude through a single round trip of the SDK control protocol. The stdin payload is a `control_request` envelope that names a unique request id and the subtype `get_context_usage`. The claude process matches the subtype against its supported control surface, runs the same accounting logic the live `/context` command runs, and emits a `control_response` envelope on stdout with the same request id.

`scanForUsage` in `probe_spawn.go` walks stdout line by line looking for the matching response. Each line is one JSON envelope. The stream carries other SDK messages while the response is in flight, including hook events, session events, and any unrelated control messages claude emits, and `scanForUsage` ignores every line whose envelope shape, request id, or subtype does not match. The function returns the parsed snapshot when it finds the matching success response, returns a typed error when it finds a matching error response, and returns a stream-closed error when stdout reaches EOF without a match.

The scanner buffer is sized to 8 MiB so a /context payload carrying many categories, long category names, or large memory-file accounting still fits in a single scan token. Sessions that approach the 1M-token context window can produce control responses several hundred kilobytes long, and the buffer headroom keeps the parse from failing on transcripts the planner most needs to compact.

## `ProbeOptions.WorkDir`

The probe spawns claude with an explicit `cmd.Dir` set from `ProbeOptions.WorkDir`. The directory has to match the directory the original interactive session ran from, because claude resolves memory files such as `CLAUDE.md`, skills, agents, and other workspace-relative configuration from its current working directory. A probe spawned from the wrong cwd loads a different set of these inputs than the live session uses, so the snapshot's category breakdown drifts from the live session's actual `/context` view.

The mismatch produces planner errors that the user feels as inconsistency rather than as a hard failure. The planner may commit a result that keeps more or less than it should, because the probe under-counts or over-counts the static overhead and its category totals diverge from what the live session sees. The `work_dir` field on every probe log line is the recovery signal: a developer comparing probe logs to the live session's project dir can spot the divergence even when the planner has already silently committed against a wrong baseline.

## The CandidateProber JSONL spawn

`claudeCandidateProber.CountCandidate` in `candidate.go` implements the generic `contextusage.CandidateProber` contract by writing the candidate transcript to a disposable JSONL inside the live session's project directory, spawning a probe against a fresh session id pointed at that candidate, and returning the Messages-category token count from the snapshot.

Writing the candidate next to the live session is deliberate. Claude resolves project context, memory files, skills, and the system prompt from the file system, so a candidate placed inside the same project directory as the live session inherits the same workspace resolution the live session uses. A candidate placed under `/tmp` would resolve to a different workspace context, and its `/context` numbers would not be comparable to the numbers the user sees on resume.

The candidate file gets a fresh UUID for its session id and the same UUID for its filename, written with 0o600 permissions to match Claude's own session file mode. The probe runs with `ForkSession: true` so claude isolates the candidate as a disposable spawn rather than treating it as a real session it should remember. The candidate file is unlinked on every return path, including error paths, so a leaked candidate file is bounded to a single crashing probe rather than accumulating across runs.

The Messages category is the only one the planner reads from the candidate snapshot. Every other `/context` category (System prompt, System tools, MCP tools, Memory files, Skills, Custom agents, and so on) sits in the planner's static overhead bucket and stays constant across candidates, so the planner only needs the Messages delta to compare candidates against the target.