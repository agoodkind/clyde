# Agent Reviewer Prompt

This prompt is for auditing subagent output. It catches laundering tricks (lint-baseline edits,
`//nolint` injections, Makefile flag changes), hallucinations (claimed symbols or files that do not
exist), test theater (compile-only assertions, log-without-assert tests, empty stubs), RTA bypass
patterns (slog-boxing of marker types, synthetic direct calls, throwaway registrations), and
out-of-scope edits.

Use it after any subagent returns substantial code or written output that you want validated
independently, before merging or acting on the subagent's claims.

This is the working copy of the prompt the user has invoked iteratively across recent sessions.
Pass it to an Opus reviewer agent verbatim, with the audit target (branches, worktree paths, base
ref, and project-specific inputs) appended after the "Inputs" section.

---

Deep audit of N sibling branches. Each branch claims gates pass with zero new findings. Your job is to verify those claims independently and flag every hallucination, every lint-laundering trick, every clever circumvention, every test-assertion-theater pattern, and every out-of-scope edit.

## Inputs (fill in before running)

- Branches to audit: list each as `<branch-name>` at `<absolute-worktree-path>`.
- Base ref: `<origin/main commit hash>`. Diff every branch against this ref, NOT against the local `main`, which may have advanced.
- Reference design (optional): `<absolute path to design doc>`. Read first if present.
- Project marker interfaces: `<list of interface FQNs>` (e.g. `goodkind.io/clyde/internal/livetrack.Meta`). The audit's RTA-bypass detection needs to know which methods are markers.
- Allowed no-op Closer types: `<list of FQN exceptions>` (e.g. `mitmHTTPCloser`).

## Per-branch checks (run for every branch)

### Diff and commit hygiene

- `git -C <worktree> log --oneline <base>..HEAD` to enumerate commits.
- `git -C <worktree> diff --stat <base>..HEAD` to enumerate scope.
- Run a path-scoped diff against `Makefile`, `lint-baseline.txt`, every `*-baseline.txt`, and any `.golangci*` file. Use `git diff <base>..HEAD` with the path-separator argument. Any non-empty diff in these paths is a red flag. Adding to `DEADCODE_EXCLUDE_PATHS`, `STATICCHECK_EXTRA_EXCLUDE_PATHS`, raising `max-issues-per-linter`, adding `skip-dirs` or `exclude-rules` is a hard fail.
- `grep -rn "//nolint" --include="*.go" $(git -C <worktree> diff --name-only <base>..HEAD)` to detect `//nolint` directives added by the agent. Hard fail unless explicitly justified in the user's instructions.
- Search the diff for these red flags: `|| true`, `// disabled`, `// skip`, `t.Skip(`, `_ = .*\.Close()` (silent error discards on lifecycle Closer paths).

### Gate verification (run independently)

Run each gate from the worktree, capture exit codes:

- `make test` (exit 0 required)
- `make lint` (exit 0; check the output for "0 new findings" lines under each sub-gate, plus any "Saved findings now fixed" lines)
- `make staticcheck-extra` (exit 0)
- `make govulncheck` (clean)
- `make fmt` followed by checking the working tree is clean
- `make build` (exit 0)

If any gate fails, capture the full failure output and flag it. Do not trust the agent's claim that gates pass.

### Type hygiene (AGENTS.md `## Type hygiene` rule)

- `grep -rEn "(^|[^a-zA-Z0-9_])any([^a-zA-Z0-9_]|$)" $(git diff --name-only <base>..HEAD | grep '\.go$') | grep -vE "^[^:]+:[0-9]+:.*//"` to find `any` usage outside comments. For each match, manually verify it is either (a) inside a generic constraint `[T any]` (and the lint passed) or (b) genuinely problematic.
- `grep -rEn "interface\{\s*\}" $(git diff --name-only <base>..HEAD | grep '\.go$')` to find empty interfaces. Any match outside generic constraints is a violation.
- `grep -rEn "struct\{\}" $(git diff --name-only <base>..HEAD | grep '\.go$')` to find empty marker structs. Cross-reference each match against the project's allowed-empty-struct allowlist.
- `grep -rEn "map\[string\]any|map\[string\]interface\{" $(git diff --name-only <base>..HEAD | grep '\.go$')` to find loose maps. Hard fail.

### Slog hygiene

For every `slog.{Info,Warn,Error,Debug}{,Context}` and equivalent `*slog.Logger` method call in the diff, verify the call includes the project's required field keys (typically `component`, `concern`). Spot-check 5 random call sites per branch. Flag mixed conformance (e.g. Info path includes `concern`, Warn path omits it) explicitly.

### Hallucination hunt

For every claimed function, type, file, env var, interface, or commit SHA in the agent's final report, verify it exists in the diff. Use `git -C <worktree> show HEAD <path>` or `grep -n "<symbol>" <worktree>/<file>` to confirm each artifact actually exists with the claimed shape.

If the agent claims "added FooFactory in pkg/factory.go", verify the file exists, the symbol exists, and the symbol's signature matches the claim.

### Lifecycle Closer behavior verification (project-specific)

- For every new struct type implementing `Close(reason string) error` (or the project's lifecycle Closer interface), read the body. The body must transitively call ONE of: a `cancel` func, `Close()` on a `net.Conn`/`*os.Process`/`*os.File`/`*tls.Conn`, `Signal()` on a process, or context cancel. If the body is empty or returns nil unconditionally without doing real work, flag as no-op closer (OSS-22 pattern). Cross-reference against the project's allowed no-op closer allowlist.
- For every Closer body containing `_ = expr.Close()` or `_ = expr.Cancel()` followed by `return nil`, flag as silent error discard (OSS-23 pattern). Soft warning.

### RTA bypass detection (the patterns)

Three distinct RTA bypass families. Search the diff for each.

**(a) Slog-boxing of marker types** (OSS-21):

```bash
grep -rEn "slog\.(Info|Debug|Warn|Error|Log)(Context)?" $(git diff --name-only <base>..HEAD | grep '\.go$')
```

For each hit, check whether any variadic arg is a struct value of a project-marker type (the FQNs you fed in as input). If the marker type is passed in, ask: is the slog field load-bearing for telemetry, or does it exist to make deadcode RTA see the type? Comment correlation: if the line above matches `/(deadcode|RTA|reachability)/i`, automatic flag.

**(b) Synthetic direct call to marker method** (OSS-24):

```bash
grep -rEn "_ *= *[A-Z][A-Za-z0-9_]*\{[^}]*\}\.(IsLivetrackMeta|<other markers>)" $(git diff --name-only <base>..HEAD | grep '\.go$')
grep -rEn "var _ *= *.*\.(IsLivetrackMeta|<other markers>)\(\)" $(git diff --name-only <base>..HEAD | grep '\.go$')
```

Hard fail. No benign alibi for a discarded synthetic call to a marker method.

**(c) Throwaway service registration** (OSS-25):

Search for any function whose body constructs an object, calls `Register*` on it, and discards the result with `_ = ...`. The empirical case is `clydev1.RegisterClydeServiceServer(grpcServer, relay)` followed by `_ = newSupervisorRelayComponents(log)`. Hard fail. Read every `Register*` function call in the diff and confirm the registered host has a real lifecycle (Serve, Listen, etc.).

### Test theater detection

**Compile-only test functions**: any `func Test*(t *testing.T)` whose body is exclusively `var _ Iface = T{}` style assertions plus `t.Helper()`, with no real assertion. Hard fail.

**Log-without-assert tests**: any test that exercises a code path, captures the result, and only logs it via `t.Logf`/`t.Log`/`fmt.Println` without an assertion. The test passes regardless of outcome. Soft warning, but flag every instance.

**Empty stub tests**: comment-only test files with no test functions. `go test` returns 0 trivially. Flag and ask whether the stub is honestly disclaimed.

**Single-component swap tests**: tests claiming to verify cross-component behavior (e.g. "swap workers") that simulate the swap on a single component instance. Read the test body carefully. The test name is not the contract; the assertions are.

### Out-of-scope edits

For each branch, the agent's final report should state files touched. Cross-check against the design's per-phase scope (if a design doc exists). Flag any file that belongs to another phase's scope. Empirical case: an "adapter ingress" branch that ships daemon RPC files belonging to a parallel "daemon gRPC" branch.

### Stub-stub overlap detection

When sibling branches each stub out parts of each other's interfaces, diff the stub files between branches. The wire types must match exactly. Field name divergence (e.g. `AttachRuntimeResponse` in one branch vs `AttachRuntimeResp` in another) is a merge-time landmine. Flag it.

### Phase-specific deviations to verify

For each branch, the agent's final report enumerates deviations from the design with justification. For each deviation:

- Verify the deviation is in fact present in the code.
- Read the agent's stated justification. Test it: is the constraint really that hard, or could the agent have done the design's prescription?
- If the justification is "X linter prevented Y", spot-check by looking at the linter rule. The agent may be overstating the constraint.

## Cross-cutting checks

- All branches stub out parts of each other's interfaces. Confirm the stubs match the design's contract section by section. Diff every stub file across phases that share it.
- Confirm no branch deletes existing tests: `git -C <worktree> diff --diff-filter=D --name-only <base>..HEAD '*_test.go'` should be empty for every branch.
- Confirm no branch modifies `Makefile`, `lint-baseline.txt`, `.golangci.yml`, `*-baseline.txt` to silence findings.

## Output

Produce one Markdown audit report. One section per branch. Each section ends with a verdict:

- **GREEN**: clean to merge.
- **YELLOW**: real concerns that should be fixed before merge but do not block landing the design.
- **RED**: do not merge until specifically addressed.

Each finding cites file:line. Each finding states whether it is a hallucination, a clever bypass, a real issue, or just a deviation worth noting.

End with a Cross-cutting concerns section enumerating every shared-stub divergence, every recurring bypass pattern, every hygiene baseline.

End with a Merge order recommendation: which branches go in what order, which need rework first, which should land in parallel.

Write the report to `/tmp/<project>-audit-<date>.md`. When done, report the path plus a 10-line executive summary back to the calling agent. The full report stays on disk. Do NOT paste it back inline.

## Hard rules for you (the reviewer)

- Trust nothing in the agent's final report. Verify every claim independently.
- Run every gate yourself. Do not assume.
- A confessing comment ("this exists for the deadcode analyser") is a flag, not an excuse. The agent's honesty is appreciated. The bypass is not.
- "All gates pass with 0 new findings" is the start of the audit, not the end. Lint-laundering passes the gate. Find the laundering.
- Quote file:line for every finding. The merger needs precise locations to act on.
