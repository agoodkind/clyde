# Reorient compaction token split Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Select the retained half of a Claude `/compact` request by token budget instead of by message count, and read the budget and the content arguments from the text typed after `/compact`.

**Architecture:** `internal/reorientinject` keeps the MITM hook and the orchestration, and it speaks only primitives, `conversation.ExportOptions`, and `tokencount.Counter`. `internal/adapter/anthropic` owns the Messages decode, the per-message text, the block-level truncation, and the synthetic `tool_result`. `internal/providers/claude/mitmcontrib` owns the compaction prompt predicate and the `/compact` argument extraction. A new leaf package `internal/conversation/exportargs` declares the export argument set once, and both `clyde conversation export` and `/compact` read that declaration.

**Tech Stack:** Go 1.27, `github.com/spf13/pflag`, `internal/tokencount`, `internal/mitm` request and response hook seam.

**Spec:** [2026-09-19-reorient-compaction-token-split-design.md](../specs/2026-09-19-reorient-compaction-token-split-design.md)

## Global Constraints

- Worktree: `/Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model`, branch `worktree-sentinel-preserve-model`. Run every `git` command with `git -C <worktree>`.
- Run `CGO_ENABLED=1` for every build and test. `make check` is lint only; run `make test` separately before claiming a task is green.
- Package-scoped tests run as `CGO_ENABLED=1 go test ./internal/<pkg>/... -count=1`. Bare `go build`, `go vet`, `gofmt`, and repo-scope `go test ./...` are blocked by the agent-gate hook; use `make build`, `make lint`, `make fmt`, `make test`.
- No em dashes or en dashes in any file this plan creates or edits.
- Generic Boundary, P0 in `AGENTS.md` as of commit `34827c416`: `internal/reorientinject` must import no Anthropic wire type and construct no Anthropic literal. It may call provider functions that accept and return primitives, `conversation.ExportOptions`, and `tokencount.Counter`.
- Type Hygiene, `AGENTS.md`: no `any`, no `interface{}`, no `map[string]any`, no empty marker structs. `json.RawMessage` stays allowed where it already is, at the smallest edge.
- Commit each task separately, signed: `git -C <worktree> commit -S`. End every commit message with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- Verified import directions, from `go list -deps`: `internal/clispec` imports `internal/daemon`; `internal/daemon` imports `internal/reorientinject`; `internal/daemon` does not import `internal/clispec`. `internal/reorientinject`, `internal/daemon`, `internal/adapter/anthropic`, and `internal/providers/claude/mitmcontrib` therefore cannot import `internal/clispec`, which is why Task 1 creates a leaf package.
- Verified import directions, from `go list -deps`: `internal/conversation` does not import `internal/adapter/anthropic`; `internal/adapter/anthropic` imports `internal/conversation`; `internal/providers/claude/mitmcontrib` imports `internal/conversation` and imports neither `internal/adapter/anthropic` nor `internal/reorientinject`.

## Out of scope

- The native Codex compaction path in `internal/adapter/codex`. It keeps its own split and its own sizing. CLYDE-742 covers it.
- The `reorient_*` settings stay in `internal/config/mitm_config.go`, because `rawCompactionMaxBytes` and `normalizedRecentFraction` in `internal/adapter/codex` read the same keys. Only the Claude path stops reading them.
- `internal/hookspec` and its `SnapshotStore`. `runReorientStopFollowup` calls `SnapshotStore.Consume` to decide whether Cursor emits its Tier 1 note, and that consumer is unrelated to the Tier 2 content provider Task 6 deletes. Task 6 ends with a test run over `internal/hookspec` to prove it still passes.

## Coordination

The session named `clyde-compaction-header`, on branch `worktree-reorient-compaction-class`, is changing how `internal/reorientinject` decides a request is a compaction. It adds `mitm.RequestPurpose`, a `mitm.RequestClassifier` provider extension, and a classifier in `internal/providers/claude/mitmcontrib`, then requires `RequestPurposeCompaction` before `MatchRequestResponse` matches. It edits `MatchRequestResponse`, that function's doc comment, and the `compactPromptSignature` comment.

That branch lands first. Start Task 5 only after it merges, and rebase this branch onto it. Tasks 1 through 4 touch no file that branch touches and can start immediately.

Its measurements, unverified here and attributed to that session:

- A real compaction request sets the request-class header `X-Claude-Code-Request-Class` to `compaction`, for both the manual and the automatic trigger.
- `/compact` sends `X-Claude-Code-Compaction: manual` and `X-Cc-Compaction-Request: manual`, capture row 363783. The automatic trigger sends `reactive`, not `auto`, capture row 366501. Match the request class, not the compaction header value.
- The prompt signature sat at offset 379 inside the last user message of a real compaction request, so no prefix test works.

Task 7 uses the request-class header to find a real compaction request in the capture store.

## Open correction to the spec

The spec's boundary message table states that a cut inside a `tool_use` input retains "the call truncated at the cut, plus a synthetic result for that call id". A `tool_use` block's `input` is a JSON object, and cutting its bytes at an arbitrary offset produces invalid JSON that Anthropic rejects. This plan therefore truncates at block granularity for `tool_use`: the upstream request retains every whole block before the cut and drops the partial `tool_use` block, and the synthetic `tool_result` covers any `tool_use` block the upstream request still retains after its result was deleted. A partial `text` block and a partial `tool_result` content string are still cut at the text position, because both are text valued.

Task 4 implements that rule. Alex confirms or replaces it before Task 4 starts.

## File structure

| Path | Responsibility |
| --- | --- |
| `internal/conversation/exportargs/exportargs.go` | Declares the export body-shaping argument set once. Registers it on a `pflag.FlagSet` and resolves a parsed set into `conversation.ExportOptions`. |
| `internal/clispec/conversation_export.go` | Renders the same declaration as terminal flags and MCP properties, and adds the terminal-only destination flags. |
| `internal/providers/claude/mitmcontrib/compact_prompt.go` | Reports whether a decoded text is Claude Code's compaction prompt, and extracts the `/compact` arguments from it. |
| `internal/adapter/anthropic/compaction_split.go` | Decodes the Messages array, returns one message's text, truncates one message at a text position, appends the synthetic `tool_result`, and re-encodes the trimmed request body. |
| `internal/reorientinject/hook.go` | Matches the request, orchestrates the split, and injects the retained text into the summary response. |
| `internal/reorientinject/split.go` | Chooses the boundary message from the per-message token counts. |
| `internal/daemon/runtime.go` | Builds the token counter from the `[export]` settings and registers the hook. |

---

### Task 1: Declare the export argument set in one leaf package

**Files:**
- Create: `internal/conversation/exportargs/exportargs.go`
- Create: `internal/conversation/exportargs/exportargs_test.go`

**Interfaces:**
- Consumes: `conversation.ExportOptions`, `conversation.ResolveContentKinds`, `conversation.NormalizeCompactionExportOptions`, `conversation.ContentKindSelectorValues`, `conversation.WhitespaceMode`.
- Produces:
  - `type Kind uint8` with `KindString`, `KindInt`, `KindBool`, `KindEnum`, `KindEnumList`.
  - `type Descriptor struct { Canonical, Description string; Kind Kind; Values []string; DefaultStr string; DefaultInt int; DefaultBool bool; Shortcut bool }`
  - `func Descriptors() []Descriptor`
  - `type Selections struct { Options conversation.ExportOptions; Kinds []string; Whitespace []conversation.WhitespaceMode }`
  - `func Apply(sel *Selections, canonical string, text string) error`
  - `func Resolve(sel Selections) (conversation.ExportOptions, error)`
  - `func Parse(args []string) (conversation.ExportOptions, error)`
  - `func FlagName(canonical string) string`

`Descriptor.Shortcut` marks the terminal-only sugar flags (`chat`, `thinking`, `tools`, `preserve`, `dense`, and the rest). `Apply` writes one already-decoded value into `Selections` by canonical name. `Resolve` runs the whitespace choice, `ResolveContentKinds`, and `NormalizeCompactionExportOptions` in that order and returns the finished options. `Parse` registers every descriptor on a fresh `pflag.FlagSet`, parses `args`, calls `Apply` for each flag the parse changed, then returns `Resolve`.

- [ ] **Step 1: Write the failing test**

Create `internal/conversation/exportargs/exportargs_test.go`:

```go
package exportargs_test

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/exportargs"
)

func TestParseReadsMaxTokensAndContentKinds(t *testing.T) {
	t.Parallel()
	options, err := exportargs.Parse([]string{"--max-tokens", "500k", "--only", "chat"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if options.MaxTokens != "500k" {
		t.Fatalf("MaxTokens = %q, want 500k", options.MaxTokens)
	}
	if !options.Content.Has(conversation.ContentKindChat) {
		t.Fatal("chat kind missing from parsed content")
	}
	if options.Content.Has(conversation.ContentKindToolOutputs) {
		t.Fatal("tool outputs selected without an argument asking for them")
	}
}

func TestParseAcceptsEveryDeclaredArgument(t *testing.T) {
	t.Parallel()
	for _, descriptor := range exportargs.Descriptors() {
		value := sampleValue(descriptor)
		args := []string{"--" + exportargs.FlagName(descriptor.Canonical)}
		if value != "" {
			args = append(args, value)
		}
		if _, err := exportargs.Parse(args); err != nil {
			t.Fatalf("Parse(%v): %v", args, err)
		}
	}
}

func sampleValue(descriptor exportargs.Descriptor) string {
	switch descriptor.Kind {
	case exportargs.KindBool:
		return ""
	case exportargs.KindInt:
		return "1"
	case exportargs.KindEnum, exportargs.KindEnumList:
		return descriptor.Values[0]
	default:
		return "x"
	}
}

func TestParseRejectsUnknownArgument(t *testing.T) {
	t.Parallel()
	if _, err := exportargs.Parse([]string{"--not-a-flag"}); err == nil {
		t.Fatal("unknown flag parsed without an error")
	} else if !strings.Contains(err.Error(), "not-a-flag") {
		t.Fatalf("error = %v, want the flag name in the message", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/conversation/exportargs/... -count=1`
Expected: FAIL, `no required module provides package goodkind.io/clyde/internal/conversation/exportargs`.

- [ ] **Step 3: Write the declaration and the parser**

Create `internal/conversation/exportargs/exportargs.go`. Copy the canonical names, descriptions, defaults, and enum value lists verbatim from `exportParams()` in `internal/clispec/conversation_export.go:65-141`, minus `output` and `stdout`, which name a destination rather than shape the body:

```go
// Package exportargs declares the arguments that shape a conversation export
// body. The terminal command, the MCP tool, and the /compact argument text all
// read this declaration, so an argument added here reaches every surface.
package exportargs

import (
	"fmt"
	"io"

	"github.com/spf13/pflag"
	"goodkind.io/clyde/internal/conversation"
)

// Kind is the value shape of one declared argument.
type Kind uint8

const (
	KindString Kind = iota
	KindInt
	KindBool
	KindEnum
	KindEnumList
)

// Descriptor declares one argument. Shortcut marks terminal-only sugar that
// selects a content kind or a whitespace mode.
type Descriptor struct {
	Canonical   string
	Description string
	Kind        Kind
	Values      []string
	DefaultStr  string
	DefaultInt  int
	DefaultBool bool
	Shortcut    bool
}

// Selections accumulates decoded argument values before Resolve turns them
// into finished export options.
type Selections struct {
	Options    conversation.ExportOptions
	Kinds      []string
	Whitespace []conversation.WhitespaceMode
}

// FlagName returns the dash-spelled terminal flag for a canonical name.
func FlagName(canonical string) string { ... }

// Descriptors returns the declared argument set in terminal help order.
func Descriptors() []Descriptor { ... }

// Apply writes one decoded value into sel by canonical name.
func Apply(sel *Selections, canonical string, text string) error { ... }

// Resolve turns accumulated selections into finished export options. It
// rejects a request that names two different whitespace modes, and it runs the
// same content-kind and compaction normalization the terminal command runs.
func Resolve(sel Selections) (conversation.ExportOptions, error) { ... }

// Parse reads an argument list and returns the finished export options.
func Parse(args []string) (conversation.ExportOptions, error) {
	set := pflag.NewFlagSet("export", pflag.ContinueOnError)
	set.SetOutput(io.Discard)
	register(set)
	if err := set.Parse(args); err != nil {
		return conversation.ExportOptions{}, fmt.Errorf("parse export arguments: %w", err)
	}
	sel := Selections{Options: defaultOptions(), Kinds: nil, Whitespace: nil}
	var applyErr error
	set.Visit(func(flag *pflag.Flag) {
		if applyErr != nil {
			return
		}
		applyErr = Apply(&sel, canonicalOf(flag.Name), flag.Value.String())
	})
	if applyErr != nil {
		return conversation.ExportOptions{}, applyErr
	}
	return Resolve(sel)
}
```

Move the whitespace conflict check out of `resolveExportWhitespace` in `internal/clispec/export_whitespace.go` into `Resolve`, preserving its exact error text so the terminal message does not change.

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/conversation/exportargs/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model add internal/conversation/exportargs
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model commit -S -m "$(cat <<'EOF'
Declare the conversation export argument set in internal/conversation/exportargs

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Render the terminal and MCP export flags from the one declaration

**Files:**
- Modify: `internal/clispec/conversation_export.go:65-141`
- Modify: `internal/clispec/export_whitespace.go`
- Modify: `internal/clispec/export_flags_test.go`

**Interfaces:**
- Consumes: `exportargs.Descriptors`, `exportargs.Apply`, `exportargs.Resolve`, `exportargs.Selections` from Task 1.
- Produces: no new exported symbol. `exportParams()` keeps its signature `[]Param[exportInput]`.

`exportInput` gains one field, `Selections exportargs.Selections`, and its existing `Options`, `Kinds`, and `WhitespaceSelections` fields are deleted in favor of it. Every reference to `in.Options` becomes `in.Selections.Options`.

- [ ] **Step 1: Write the failing test**

Add to `internal/clispec/export_flags_test.go`:

```go
// TestExportParamsCoverEveryDeclaredArgument fails when a new export argument
// reaches one surface only. The terminal and MCP flags are rendered from the
// same declaration the /compact argument parser reads, so an argument added to
// exportargs must appear here with no further edit.
func TestExportParamsCoverEveryDeclaredArgument(t *testing.T) {
	t.Parallel()
	rendered := map[string]bool{}
	for _, param := range exportParams() {
		rendered[param.Canonical] = true
	}
	for _, descriptor := range exportargs.Descriptors() {
		if !rendered[descriptor.Canonical] {
			t.Errorf("export argument %q is declared but not rendered as a flag", descriptor.Canonical)
		}
	}
	for _, name := range []string{"output", "stdout"} {
		if !rendered[name] {
			t.Errorf("terminal destination flag %q is missing", name)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/clispec/... -run TestExportParamsCoverEveryDeclaredArgument -count=1`
Expected: FAIL, `undefined: exportargs`.

- [ ] **Step 3: Build the params from the descriptors**

Replace the body of `exportParams()` with a loop over `exportargs.Descriptors()` that maps each descriptor onto the matching `Param` constructor, then appends the two destination flags:

```go
func exportParams() []Param[exportInput] {
	descriptors := exportargs.Descriptors()
	params := make([]Param[exportInput], 0, len(descriptors)+2)
	for _, descriptor := range descriptors {
		params = append(params, exportParamFor(descriptor))
	}
	return append(params, exportOutputParam(), exportStdoutParam())
}

// exportParamFor renders one declared export argument as a terminal flag and an
// MCP property. The bind closure writes the decoded value back through
// exportargs.Apply, so the declaration owns the meaning of every name and this
// file owns only the rendering.
func exportParamFor(descriptor exportargs.Descriptor) Param[exportInput] {
	apply := func(in *exportInput, text string) {
		if err := exportargs.Apply(&in.Selections, descriptor.Canonical, text); err != nil {
			slog.Warn("cli.conversation.export_argument_invalid",
				"concern", "cli.conversation", "component", "cli",
				"argument", descriptor.Canonical, "err", err)
		}
	}
	switch descriptor.Kind {
	case exportargs.KindBool:
		param := BoolParam(descriptor.Canonical, descriptor.Description, descriptor.DefaultBool,
			func(in *exportInput, v bool) {
				if v {
					apply(in, "true")
				}
			})
		param.CLIOnly = descriptor.Shortcut
		return param
	case exportargs.KindInt:
		return IntParam(descriptor.Canonical, descriptor.Description, descriptor.DefaultInt,
			func(in *exportInput, v int) { apply(in, strconv.Itoa(v)) })
	case exportargs.KindEnum:
		return EnumParam(descriptor.Canonical, descriptor.Description, descriptor.DefaultStr, descriptor.Values,
			func(in *exportInput, v string) { apply(in, v) })
	case exportargs.KindEnumList:
		return EnumListParam(descriptor.Canonical, descriptor.Description, descriptor.Values, true,
			func(in *exportInput, v []string) { apply(in, strings.Join(v, ",")) })
	default:
		return StringParam(descriptor.Canonical, descriptor.Description, descriptor.DefaultStr, false,
			func(in *exportInput, v string) { apply(in, v) })
	}
}
```

Replace the option-building half of the export `Prepare` with one call:

```go
Prepare: func(in exportInput) (exportPayload, error) {
	options, err := exportargs.Resolve(in.Selections)
	if err != nil {
		slog.Warn("cli.conversation.export_arguments_invalid", "concern", "cli.conversation", "component", "cli", "err", err)
		return exportPayload{}, err
	}
	if in.Stdout && in.OutputPath != "" && in.OutputPath != "-" {
		return exportPayload{}, fmt.Errorf("select output destination: --stdout cannot be combined with --output %q", in.OutputPath)
	}
	return exportPayload{
		ConversationID: in.ConversationID,
		Options:        options,
		OutputPath:     in.OutputPath,
		Stdout:         in.Stdout || in.OutputPath == "-",
	}, nil
},
```

Delete `resolveExportWhitespace` and `recordWhitespaceSelection` from `internal/clispec/export_whitespace.go`, because `exportargs.Resolve` now owns that choice. Keep the file if it holds other helpers.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/clispec/... -count=1`
Expected: PASS, including `TestConversationAlignment` and every existing export flag test.

- [ ] **Step 5: Commit**

```bash
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model add internal/clispec
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model commit -S -m "$(cat <<'EOF'
Render the export flags from the exportargs declaration in internal/clispec

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Read the compaction prompt and its arguments in the Claude provider package

**Files:**
- Create: `internal/providers/claude/mitmcontrib/compact_prompt.go`
- Create: `internal/providers/claude/mitmcontrib/compact_prompt_test.go`

**Interfaces:**
- Consumes: `exportargs.Parse` and `exportargs.Descriptors` from Task 1.
- Produces:
  - `func IsCompactionPrompt(text string) bool`
  - `func CompactArguments(text string) (conversation.ExportOptions, bool, error)`

Both accept an already-decoded string, so this package declares no Anthropic wire type.

Claude Code appends the text typed after `/compact` to the compaction prompt below a line reading `Additional Instructions:`, then appends a reminder block starting with `REMINDER: Do NOT call any tools.` (claude-code source, `src/services/compact/prompt.ts:269-303`). `CompactArguments` reads the region between those two markers, splits it into fields, consumes the leading fields that name a declared argument, and stops at the first field that does not. It returns `ok=false` when the marker is absent, and it returns an error only when a recognized argument carries an unusable value.

- [ ] **Step 1: Write the failing test**

Create `internal/providers/claude/mitmcontrib/compact_prompt_test.go`:

```go
package mitmcontrib

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

const compactPromptFixture = "Your task is to create a detailed summary of the conversation so far.\n"

// promptWithInstructions builds the prompt shape Claude Code sends when the
// operator types text after /compact.
func promptWithInstructions(instructions string) string {
	return compactPromptFixture +
		"\n\nAdditional Instructions:\n" + instructions +
		"\n\nREMINDER: Do NOT call any tools."
}

func TestCompactArgumentsReadsLeadingFlags(t *testing.T) {
	t.Parallel()
	options, ok, err := CompactArguments(promptWithInstructions("--max-tokens 50k --only chat"))
	if err != nil || !ok {
		t.Fatalf("CompactArguments: ok=%v err=%v", ok, err)
	}
	if options.MaxTokens != "50k" {
		t.Fatalf("MaxTokens = %q, want 50k", options.MaxTokens)
	}
	if !options.Content.Has(conversation.ContentKindChat) {
		t.Fatal("chat kind missing")
	}
}

func TestCompactArgumentsStopsAtProse(t *testing.T) {
	t.Parallel()
	options, ok, err := CompactArguments(promptWithInstructions("--max-tokens 50k focus on the test failures"))
	if err != nil || !ok {
		t.Fatalf("CompactArguments: ok=%v err=%v", ok, err)
	}
	if options.MaxTokens != "50k" {
		t.Fatalf("MaxTokens = %q, want 50k", options.MaxTokens)
	}
}

func TestCompactArgumentsIgnoresProseOnlyInstructions(t *testing.T) {
	t.Parallel()
	options, ok, err := CompactArguments(promptWithInstructions("focus on the test failures"))
	if err != nil {
		t.Fatalf("prose instructions returned an error: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true for a present instruction block")
	}
	if options.MaxTokens != "" {
		t.Fatalf("MaxTokens = %q, want empty", options.MaxTokens)
	}
}

func TestCompactArgumentsAbsentBlock(t *testing.T) {
	t.Parallel()
	if _, ok, err := CompactArguments(compactPromptFixture); ok || err != nil {
		t.Fatalf("CompactArguments(no block): ok=%v err=%v", ok, err)
	}
}

func TestIsCompactionPromptMatchesAtAnyOffset(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("earlier context\n", 40) + compactPromptFixture
	if !IsCompactionPrompt(text) {
		t.Fatal("signature at a non-zero offset did not match")
	}
	if IsCompactionPrompt("please summarize what we did") {
		t.Fatal("an ordinary request matched the compaction predicate")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/providers/claude/mitmcontrib/... -run 'Compact|IsCompaction' -count=1`
Expected: FAIL, `undefined: CompactArguments`.

- [ ] **Step 3: Write the predicate and the argument reader**

Create `internal/providers/claude/mitmcontrib/compact_prompt.go`:

```go
package mitmcontrib

import (
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/exportargs"
)

const (
	// compactPromptSignature is a distinctive substring every Claude Code
	// compaction prompt includes. It appears at an arbitrary offset inside the
	// last user message, measured at offset 379 in one captured request, so the
	// predicate searches rather than matching a prefix.
	compactPromptSignature = "Your task is to create a detailed summary of"

	// compactInstructionsMarker opens the region holding the text typed after
	// /compact (claude-code source, src/services/compact/prompt.ts:293-303).
	compactInstructionsMarker = "Additional Instructions:"

	// compactReminderMarker opens the block Claude Code appends after that text.
	compactReminderMarker = "REMINDER: Do NOT call any tools."
)

// IsCompactionPrompt reports whether text is a Claude Code compaction prompt.
func IsCompactionPrompt(text string) bool {
	return strings.Contains(text, compactPromptSignature)
}

// CompactArguments returns the export options the operator typed after
// /compact. The second result is false when the prompt includes no instruction
// block. Prose and PreCompact hook instructions occupy the same block, so the
// reader consumes the leading fields that name a declared argument and treats
// the rest as content. An unusable value for a recognized argument returns an
// error, and the caller proceeds on the configured budget.
func CompactArguments(text string) (conversation.ExportOptions, bool, error) {
	_, after, found := strings.Cut(text, compactInstructionsMarker)
	if !found {
		return conversation.ExportOptions{}, false, nil
	}
	if before, _, cut := strings.Cut(after, compactReminderMarker); cut {
		after = before
	}
	options, err := exportargs.Parse(leadingArguments(strings.Fields(after)))
	if err != nil {
		return conversation.ExportOptions{}, true, err
	}
	return options, true, nil
}

// leadingArguments returns the prefix of fields that names declared arguments
// and their values. It stops at the first field that starts no flag and answers
// no preceding flag.
func leadingArguments(fields []string) []string { ... }
```

Implement `leadingArguments` against `exportargs.Descriptors()`: a field that spells a declared flag is an argument; the field after it is that argument's value unless the descriptor is `KindBool`; any other field ends the prefix.

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/providers/claude/mitmcontrib/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model add internal/providers/claude/mitmcontrib
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model commit -S -m "$(cat <<'EOF'
Read the compaction prompt and its compact arguments in mitmcontrib

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Decode, measure, and truncate the Messages request in the Anthropic package

**Files:**
- Create: `internal/adapter/anthropic/compaction_split.go`
- Create: `internal/adapter/anthropic/compaction_split_test.go`

**Interfaces:**
- Consumes: `NormalizeContent` and the neutral `content.Part` kinds already in `internal/adapter/anthropic/content.go:54-95`.
- Produces:
  - `type CompactionRequest struct { ... }` with unexported fields.
  - `func DecodeCompactionRequest(body []byte) (*CompactionRequest, error)`
  - `func (r *CompactionRequest) MessageCount() int`
  - `func (r *CompactionRequest) Role(index int) string`
  - `func (r *CompactionRequest) Text(index int) string`
  - `func (r *CompactionRequest) SessionID() string`
  - `func (r *CompactionRequest) TrimToBoundary(boundary int, head string) ([]byte, error)`

`Text` returns the flattened text of one message's neutral parts, which is what the counter measures and what the injection renders. `TrimToBoundary` returns the rewritten request body: it retains messages `[0, boundary)` unchanged, rewrites message `boundary` to the blocks that fit `head`, deletes the messages between the boundary and the instruction region, retains the instruction region unchanged, and appends a synthetic `tool_result` for every retained `tool_use` id that lost its result. It preserves every other top-level field verbatim through `map[string]json.RawMessage`, exactly as `marshalTrimmedRequest` in `internal/reorientinject/hook.go:486-522` does today.

The validity gate stays. A token boundary changes where the cut lands and changes nothing about what Anthropic accepts, and two 400 classes are on record from 2026-07-11: an orphaned `tool_result` (fixed in `61dbfb20f`) and a `system` message followed by a non-assistant message (fixed in `0522626c0`). `TrimToBoundary` therefore keeps three rules the current split already implements, and returns an error when the result still fails:

1. Pull the instruction region's start earlier to a fixpoint so every `tool_result` inside it has its `tool_use` retained. This is `extendInstructionStart` at `internal/reorientinject/split.go:44-60`, moved into this package rather than deleted.
2. Move the boundary earlier while the boundary message's role is `system`, because the instruction region starts with a `user` message and `system` followed by `user` is the recorded 400.
3. Validate the assembled message list before returning it: every `tool_result` has its `tool_use`, every `tool_use` has a result, and no `system` message is followed by a non-assistant message except at the end. This is `validateTrim` at `internal/reorientinject/split.go:158-189`, moved into this package. A failure returns an error, and the caller forwards the request unmodified.

Block granularity inside the boundary message, per the open correction above: retain every whole block whose text ends at or before the end of `head`; cut a partial `text` block or a partial `tool_result` content string at the text position; drop a partial `tool_use` block, because a byte cut inside its `input` produces invalid JSON.

The synthetic result copies the shape Claude Code emits for a `tool_use` with no answer (claude-code source, `src/utils/messages.ts:5318-5325`): `{"type":"tool_result","tool_use_id":<id>,"content":"[Tool use was interrupted]","is_error":true}`.

- [ ] **Step 1: Write the failing test**

Create `internal/adapter/anthropic/compaction_split_test.go`:

```go
package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

const splitFixture = `{
  "model": "claude-opus-5",
  "max_tokens": 4096,
  "system": [{"type": "text", "text": "system prompt"}],
  "messages": [
    {"role": "user", "content": "start"},
    {"role": "assistant", "content": [{"type": "text", "text": "alpha\nbravo\ncharlie"}]},
    {"role": "user", "content": "run the tests"},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "toolu_01FT", "name": "Bash", "input": {"command": "go test ./..."}}]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_01FT", "content": "ok 42 tests"}]},
    {"role": "user", "content": [{"type": "text", "text": "Your task is to create a detailed summary of"}]}
  ]
}`

func decodeFixture(t *testing.T) *CompactionRequest {
	t.Helper()
	request, err := DecodeCompactionRequest([]byte(splitFixture))
	if err != nil {
		t.Fatalf("DecodeCompactionRequest: %v", err)
	}
	return request
}

func TestCompactionRequestTextFlattensBlocks(t *testing.T) {
	t.Parallel()
	request := decodeFixture(t)
	if request.MessageCount() != 6 {
		t.Fatalf("MessageCount = %d, want 6", request.MessageCount())
	}
	if got := request.Text(1); !strings.Contains(got, "bravo") {
		t.Fatalf("Text(1) = %q, want the assistant text", got)
	}
	if got := request.Text(3); !strings.Contains(got, "go test") {
		t.Fatalf("Text(3) = %q, want the tool input", got)
	}
}

func TestTrimToBoundaryPreservesTopLevelFields(t *testing.T) {
	t.Parallel()
	request := decodeFixture(t)
	body, err := request.TrimToBoundary(1, "alpha\n")
	if err != nil {
		t.Fatalf("TrimToBoundary: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode trimmed body: %v", err)
	}
	for _, field := range []string{"model", "max_tokens", "system"} {
		if _, ok := top[field]; !ok {
			t.Fatalf("trimmed body dropped %q", field)
		}
	}
}

func TestTrimToBoundaryKeepsOlderMessagesAndThePrompt(t *testing.T) {
	t.Parallel()
	request := decodeFixture(t)
	body, err := request.TrimToBoundary(1, "alpha\n")
	if err != nil {
		t.Fatalf("TrimToBoundary: %v", err)
	}
	trimmed, err := DecodeCompactionRequest(body)
	if err != nil {
		t.Fatalf("decode trimmed request: %v", err)
	}
	if trimmed.MessageCount() != 3 {
		t.Fatalf("trimmed MessageCount = %d, want 3", trimmed.MessageCount())
	}
	if got := trimmed.Text(0); got != "start" {
		t.Fatalf("trimmed Text(0) = %q, want start", got)
	}
	if got := trimmed.Text(1); !strings.Contains(got, "alpha") || strings.Contains(got, "charlie") {
		t.Fatalf("boundary message = %q, want the head only", got)
	}
	if got := trimmed.Text(2); !strings.Contains(got, "Your task is to create a detailed summary of") {
		t.Fatalf("trimmed Text(2) = %q, want the compaction prompt", got)
	}
}

func TestTrimToBoundaryAppendsSyntheticResultForARetainedCall(t *testing.T) {
	t.Parallel()
	request := decodeFixture(t)
	body, err := request.TrimToBoundary(4, "")
	if err != nil {
		t.Fatalf("TrimToBoundary: %v", err)
	}
	if count := strings.Count(string(body), "toolu_01FT"); count != 2 {
		t.Fatalf("tool id appears %d times, want the retained call plus one result", count)
	}
	if !strings.Contains(string(body), `"is_error":true`) {
		t.Fatal("synthetic result is not marked as an error")
	}
}

func TestTrimToBoundaryDropsAPartialToolUseBlock(t *testing.T) {
	t.Parallel()
	request := decodeFixture(t)
	body, err := request.TrimToBoundary(3, "go te")
	if err != nil {
		t.Fatalf("TrimToBoundary: %v", err)
	}
	if strings.Contains(string(body), "go te") {
		t.Fatal("a tool_use input was cut mid-JSON instead of dropped whole")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("partial tool_use produced invalid JSON: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/adapter/anthropic/... -run Compaction -count=1`
Expected: FAIL, `undefined: DecodeCompactionRequest`.

- [ ] **Step 3: Write the decode, the text, and the trim**

Create `internal/adapter/anthropic/compaction_split.go`. Move `anthropicSummaryRequest`, `anthropicMessage`, `anthropicMetadata`, `anthropicUserID`, `text()`, `sessionID()`, `parts()`, `toolIDs()`, `toolIDsOfParts()`, and `marshalTrimmedRequest` out of `internal/reorientinject/hook.go:294-522` and `internal/reorientinject/split.go:16-38` into this file, renaming the exported surface to the interface above. `Text` renders the neutral parts with the same per-kind rules the deleted `renderPart` applied: text and thinking return their text, a tool call renders as `[tool_use <name>] <input>`, a tool result renders as `[tool_result] <text>`, and an image renders as its flattened placeholder.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/adapter/anthropic/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model add internal/adapter/anthropic
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model commit -S -m "$(cat <<'EOF'
Own the compaction request decode and trim in internal/adapter/anthropic

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Choose the boundary by token budget in the reorient hook

Start this task only after the `worktree-reorient-compaction-class` branch merges, and rebase onto it first. That branch rewrites `MatchRequestResponse` and the `compactPromptSignature` comment in the same file.

**Files:**
- Modify: `internal/reorientinject/hook.go`
- Modify: `internal/reorientinject/split.go`
- Modify: `internal/reorientinject/hook_test.go`

**Interfaces:**
- Consumes: Task 3's `mitmcontrib.IsCompactionPrompt` and `mitmcontrib.CompactArguments`; Task 4's `anthropic.DecodeCompactionRequest`, `Text`, `SessionID`, `TrimToBoundary`; `tokencount.Counter`, `tokencount.CapToLastTokens`; `util.ParseHumanCount`.
- Produces:
  - `type Sizing struct { MaxTokens int }`, replacing the six-field struct at `internal/reorientinject/hook.go:90-110`.
  - `func New(counter tokencount.Counter, sizing Sizing) *Hook`, replacing `New(provider ContentProvider, sizing Sizing)`.
  - `func (h *Hook) Counter() tokencount.Counter`
  - `func planTokenSplit(source boundarySource, counter tokencount.Counter, budget int, promptIndex int) (tokenSplit, bool)`
  - `type tokenSplit struct { boundary int; head string; injection string; tokens int }`

Deleted in this task: `ContentProvider`, `ContentRequest`, `emptyContentProvider`, `splitPlan`, `toolPairing`, `indexToolPairs`, `extendInstructionStart`, `planSplit`, `trimKeepIndexes`, `dropOlderHalfSystemMessages`, `selectMessages`, `validateTrim`, `recentBytes`, `renderRecentMessages`, `renderBlocks`, `renderParts`, `renderPart`, `splitState`, the `Sizing` fields `ContextWindowFraction`, `BytesPerToken`, `StandardContextWindow`, `OneMillionContextWindow`, and `RecentFraction`, plus `maxBytes` and `anthropicBetaValues`.

- [ ] **Step 1: Write the failing test**

Add to `internal/reorientinject/hook_test.go`:

```go
// fixedCounter counts one token per rune so a test states its budget in exact
// characters rather than in tokenizer estimates.
type fixedCounter struct{}

func (fixedCounter) Estimate(text string) int { return len([]rune(text)) }

type stubSource struct{ texts []string }

func (s stubSource) MessageCount() int          { return len(s.texts) }
func (s stubSource) Role(index int) string      { return "user" }
func (s stubSource) Text(index int) string      { return s.texts[index] }

func TestPlanTokenSplitRetainsTheLargestOldMessageWhenItFits(t *testing.T) {
	t.Parallel()
	source := stubSource{texts: []string{
		"oldest\n",
		strings.Repeat("prior summary line\n", 10),
		"recent\n",
		"prompt\n",
	}}
	split, ok := planTokenSplit(source, fixedCounter{}, 1000, 3)
	if !ok {
		t.Fatal("planTokenSplit returned ok=false with a budget above the conversation size")
	}
	if split.boundary != 0 {
		t.Fatalf("boundary = %d, want 0 when every later message fits", split.boundary)
	}
	if !strings.Contains(split.injection, "prior summary line") {
		t.Fatal("the prior summary is missing from the injection")
	}
}

func TestPlanTokenSplitCutsInsideTheBoundaryMessage(t *testing.T) {
	t.Parallel()
	source := stubSource{texts: []string{
		"oldest\n",
		"alpha\nbravo\ncharlie\n",
		"recent\n",
		"prompt\n",
	}}
	split, ok := planTokenSplit(source, fixedCounter{}, 16, 3)
	if !ok {
		t.Fatal("planTokenSplit returned ok=false")
	}
	if split.boundary != 1 {
		t.Fatalf("boundary = %d, want 1", split.boundary)
	}
	if strings.Contains(split.head, "charlie") {
		t.Fatalf("head = %q, want the tail excluded", split.head)
	}
	if strings.Contains(split.injection, "alpha") {
		t.Fatalf("injection = %q, want the head excluded", split.injection)
	}
	if split.tokens >= 16 {
		t.Fatalf("retained tokens = %d, want strictly under 16", split.tokens)
	}
}

func TestPlanTokenSplitNeverRetainsMessageZero(t *testing.T) {
	t.Parallel()
	source := stubSource{texts: []string{"oldest\n", "recent\n", "prompt\n"}}
	split, ok := planTokenSplit(source, fixedCounter{}, 100000, 2)
	if !ok {
		t.Fatal("planTokenSplit returned ok=false")
	}
	if split.boundary != 0 {
		t.Fatalf("boundary = %d, want 0", split.boundary)
	}
	if strings.Contains(split.injection, "oldest") {
		t.Fatal("message 0 reached the injection instead of the model")
	}
}
```

Keep the existing hook tests that assert the summary span rewrite and the SSE fallback. Delete the tests that assert the count fraction, the dropped system messages, and the disk render, because this task deletes that behavior.

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/reorientinject/... -run PlanTokenSplit -count=1`
Expected: FAIL, `undefined: planTokenSplit`.

- [ ] **Step 3: Write the split**

Replace the whole of `internal/reorientinject/split.go` with the boundary source, the split, and the cut:

```go
package reorientinject

import (
	"strings"

	"goodkind.io/clyde/internal/tokencount"
)

// boundarySource is the message view the split reads. internal/adapter/anthropic
// implements it, so this package names no Anthropic type.
type boundarySource interface {
	MessageCount() int
	Role(index int) string
	Text(index int) string
}

// tokenSplit is one planned compaction rewrite. The model summarizes messages
// [0, boundary) plus head; the injection holds the rest.
type tokenSplit struct {
	boundary  int
	head      string
	injection string
	tokens    int
}

// planTokenSplit chooses the boundary message from the per-message token counts.
// It counts from the newest message toward the oldest and retains every message
// that fits whole. The boundary message is the first message that does not fit
// whole, and the retained content includes the last part of that message up to
// the remaining budget. The boundary never reaches message 0, so the model
// summarizes at least the first message in every compaction.
func planTokenSplit(source boundarySource, counter tokencount.Counter, budget int, promptIndex int) (tokenSplit, bool) {
	if budget <= 0 || promptIndex < 2 {
		return tokenSplit{}, false
	}
	used := 0
	retained := make([]string, 0, promptIndex)
	for index := promptIndex - 1; index >= 1; index-- {
		text := source.Text(index)
		count := counter.Estimate(text)
		if used+count >= budget {
			head, tail, ok := cutBoundaryMessage(text, budget-used, counter)
			if !ok {
				return tokenSplit{}, false
			}
			retained = append([]string{tail}, retained...)
			return tokenSplit{
				boundary:  index,
				head:      head,
				injection: strings.Join(retained, "\n\n"),
				tokens:    used + counter.Estimate(tail),
			}, true
		}
		used += count
		retained = append([]string{text}, retained...)
	}
	// Every message after message 0 fits the budget. Message 0 is the boundary
	// and the model receives its whole text.
	return tokenSplit{
		boundary:  0,
		head:      source.Text(0),
		injection: strings.Join(retained, "\n\n"),
		tokens:    used,
	}, true
}

// cutBoundaryMessage returns the head the model summarizes and the tail the
// injection includes. tokencount.CapToLastTokens cuts on a line boundary and
// returns a result strictly under the budget.
func cutBoundaryMessage(text string, remaining int, counter tokencount.Counter) (string, string, bool) {
	tail, _, _ := tokencount.CapToLastTokens(text, remaining, counter)
	trimmedTail := strings.TrimSuffix(tail, "\n")
	if trimmedTail == "" {
		return text, "", false
	}
	head, ok := strings.CutSuffix(strings.TrimRight(text, "\n"), trimmedTail)
	if !ok {
		return text, "", false
	}
	return head, trimmedTail, true
}
```

- [ ] **Step 4: Rewrite the match**

Replace `MatchRequestResponse` at `internal/reorientinject/hook.go:154-260` with:

```go
func (h *Hook) MatchRequestResponse(req mitm.RequestResponseHookRequest) (mitm.RequestResponseHookMatch, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.Path, messagesPathSuffix) {
		return unmatchedRequestResponseHookMatch(), nil
	}
	body, err := req.Body.Bytes()
	if err != nil {
		slog.Warn("mitm.reorient_inject.request_body_read_failed", "component", reorientInjectComponent, "concern", reorientInjectConcern, "err", err)
		return unmatchedRequestResponseHookMatch(), nil
	}
	request, err := anthropic.DecodeCompactionRequest(body)
	if err != nil {
		slog.Warn("mitm.reorient_inject.request_body_decode_failed", "component", reorientInjectComponent, "concern", reorientInjectConcern, "err", err)
		return unmatchedRequestResponseHookMatch(), nil
	}
	promptIndex, found := lastUserMessage(request)
	if !found || !mitmcontrib.IsCompactionPrompt(request.Text(promptIndex)) {
		return unmatchedRequestResponseHookMatch(), nil
	}
	if request.SessionID() == "" {
		return unmatchedRequestResponseHookMatch(), nil
	}
	budget := h.budget(request.Text(promptIndex))
	split, ok := planTokenSplit(request, h.counter, budget, promptIndex)
	if !ok {
		logSplitFallback(splitFallbackNoFit, request.MessageCount())
		return unmatchedRequestResponseHookMatch(), nil
	}
	trimmed, err := request.TrimToBoundary(split.boundary, split.head)
	if err != nil {
		logSplitFallback(splitFallbackTrimFailed, request.MessageCount())
		return unmatchedRequestResponseHookMatch(), nil
	}
	slog.Info("mitm.reorient_inject.split_planned",
		"component", reorientInjectComponent, "concern", reorientInjectConcern,
		"message_count", request.MessageCount(),
		"boundary", split.boundary,
		"budget", budget,
		"retained_tokens", split.tokens,
	)
	return mitm.RequestResponseHookMatch{
		Matched:            true,
		Transformer:        responseAppendTransformer{content: split.injection},
		RequestTransformer: staticBodyTransformer{body: trimmed},
	}, nil
}

// budget returns the token budget for one compaction. The /compact arguments
// set it; absent an argument the configured budget applies.
func (h *Hook) budget(prompt string) int {
	options, ok, err := mitmcontrib.CompactArguments(prompt)
	if err != nil {
		slog.Warn("mitm.reorient_inject.arguments_invalid", "component", reorientInjectComponent, "concern", reorientInjectConcern, "err", err)
		return h.sizing.MaxTokens
	}
	if !ok || options.MaxTokens == "" {
		return h.sizing.MaxTokens
	}
	parsed, err := util.ParseHumanCount(options.MaxTokens)
	if err != nil || parsed <= 0 {
		slog.Warn("mitm.reorient_inject.max_tokens_invalid", "component", reorientInjectComponent, "concern", reorientInjectConcern, "max_tokens", options.MaxTokens)
		return h.sizing.MaxTokens
	}
	return parsed
}
```

Replace the three fallback reasons at `internal/reorientinject/hook.go:266-270` with `splitFallbackNoFit` and `splitFallbackTrimFailed`, because the empty-render and invalid-trim paths no longer exist. Returning `unmatchedRequestResponseHookMatch()` forwards the request unmodified and leaves the response unchanged, which is spec selection requirement 9. Replace `messageTrimTransformer` with a `staticBodyTransformer` that returns the body Task 4 already built.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/reorientinject/... -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model add internal/reorientinject
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model commit -S -m "$(cat <<'EOF'
Choose the reorient compaction boundary by token budget

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Build the counter from the export settings and delete the disk provider

**Files:**
- Modify: `internal/daemon/runtime.go:256-292`
- Modify: `internal/daemon/reorient_inject_wiring_test.go`
- Modify: `clyde.example.toml`

**Interfaces:**
- Consumes: Task 5's `reorientinject.New(counter tokencount.Counter, sizing reorientinject.Sizing)` and `Hook.Counter()`.
- Produces: `func mitmRequestResponseHooks(cfg *config.Config) []mitm.RequestResponseHook`, taking the whole config instead of `config.MITMConfig`.

`newReorientInjectContentProvider` is deleted in this task. `SnapshotStore` in `internal/hookspec` is a different store with three consumers, including `runReorientStopFollowup`, and this task does not touch it. The operator's own `~/.config/clyde/config.toml` stays untouched.

- [ ] **Step 1: Write the failing test**

Add to `internal/daemon/reorient_inject_wiring_test.go`:

```go
// TestMitmHooksReorientUsesTheExportCounter asserts the reorient hook receives a
// token counter built from the [export] settings, so /compact and
// clyde conversation export count a transcript the same way.
func TestMitmHooksReorientUsesTheExportCounter(t *testing.T) {
	t.Parallel()
	cfg := config.NewConfigWithDefaults()
	cfg.MITM.ReorientSummaryInjection = true
	cfg.Export.TokenSafetyFactor = 2.0
	hooks := mitmRequestResponseHooks(cfg)
	if len(hooks) != 1 {
		t.Fatalf("hooks = %d, want 1", len(hooks))
	}
	hook, ok := hooks[0].(*reorientinject.Hook)
	if !ok {
		t.Fatalf("hook type = %T, want *reorientinject.Hook", hooks[0])
	}
	if hook.Counter() == nil {
		t.Fatal("reorient hook has no token counter")
	}
	plain := tokencount.LocalCounter(tokencount.FamilyClaude, "claude-opus-5", tokencount.Settings{SafetyFactor: 2.0})
	const sample = "one short transcript line\n"
	if hook.Counter().Estimate(sample) != plain.Estimate(sample) {
		t.Fatal("the hook counter does not match the configured export counter")
	}
}
```

Update the four existing wiring tests to pass `config.NewConfigWithDefaults()` with the matching field set instead of a bare `config.MITMConfig`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/daemon/... -run MitmHooks -count=1`
Expected: FAIL, `cannot use cfg (variable of type *config.Config) as config.MITMConfig`.

- [ ] **Step 3: Wire the counter**

```go
func mitmRequestResponseHooks(cfg *config.Config) []mitm.RequestResponseHook {
	var hooks []mitm.RequestResponseHook
	sentinel := strings.TrimSpace(cfg.MITM.Sentinel)
	actualUserSentinel := strings.TrimSpace(cfg.MITM.ActualUserSentinel)
	if sentinel != "" || actualUserSentinel != "" {
		hooks = append(hooks, sentinelinject.New(sentinel, actualUserSentinel))
	}
	if cfg.MITM.ReorientSummaryInjection {
		hooks = append(hooks, reorientinject.New(
			reorientCompactionCounter(cfg),
			reorientinject.Sizing{MaxTokens: cfg.MITM.ReorientInjectMaxTokens},
		))
	}
	return hooks
}

// reorientCompactionCounter builds the token counter the compaction split uses.
// It reads the same [export] settings clyde conversation export reads, so both
// surfaces count a transcript the same way. The Claude family is fixed because
// this hook matches only Anthropic Messages requests.
func reorientCompactionCounter(cfg *config.Config) tokencount.Counter {
	settings := tokencount.Settings{
		SafetyFactor:  cfg.Export.TokenSafetyFactor,
		CharsPerToken: cfg.Export.HeuristicCharsPerToken,
	}
	return tokencount.LocalCounter(tokencount.FamilyClaude, "", settings)
}
```

Update the one call site of `mitmRequestResponseHooks` in `internal/daemon/runtime.go` to pass `cfg`. Delete `newReorientInjectContentProvider` and every symbol only it used.

The exact counter stays out of this path: `CapToLastTokensExact` performs network I/O against the count endpoint, and the MITM request path must not block a live `/compact` on a second upstream call. The local counter is conservative by construction, `internal/tokencount/cap.go:10-14`, so the retained content stays under the budget without it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/daemon/... ./internal/hookspec/... -count=1`
Expected: PASS. The `internal/hookspec` run proves `SnapshotStore` and `runReorientStopFollowup` still pass after the content provider is deleted.

- [ ] **Step 5: Update the example configuration**

In `clyde.example.toml`, delete `reorient_recent_fraction`, `reorient_bytes_per_token`, `reorient_context_window_fraction`, `reorient_standard_context_window`, `reorient_one_million_context_window`, and `reorient_inject_max_lines` from the Claude commentary, and state that `reorient_inject_max_tokens` is the default budget that `/compact --max-tokens` overrides. Keep the keys themselves documented under the native Codex compaction path, which still reads them.

- [ ] **Step 6: Commit**

```bash
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model add internal/daemon internal/reorientinject clyde.example.toml
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model commit -S -m "$(cat <<'EOF'
Build the reorient compaction counter from the export settings

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Prove the budget live and rewrite the overview page

**Files:**
- Modify: `test/live/reorient_compact_live_test.go`
- Modify: `docs/reorient/overview.md:13-19`

**Interfaces:**
- Consumes: the sandbox daemon harness in `test/live/harness.go` and the existing `TestLiveReorientCompactSplitsAndInjects`.
- Produces: `TestLiveReorientCompactHonorsMaxTokens`.

- [ ] **Step 1: Write the failing live test**

Read `test/live/reorient_compact_live_test.go` first and reuse its harness helpers under their real names. Add a test that runs `/compact` with a 20k budget twice against one sandbox session:

```go
// TestLiveReorientCompactHonorsMaxTokens runs two compactions on one session and
// asserts the budget holds on both. The second compaction is the one that
// regressed: a count-based split assigned the first compaction's recovered
// transcript to the summarized remainder, and the retained text fell from
// 1,685,220 characters to 57,234 on 2026-09-19.
func TestLiveReorientCompactHonorsMaxTokens(t *testing.T) {
	harness := startReorientHarness(t)
	session := harness.NewClaudeSession(t)

	session.Send(t, "read internal/tokencount/cap.go and summarize it")
	session.Send(t, compactCommandWithBudget("20k"))
	first := harness.LastInjectedTranscript(t, session)
	assertUnderBudget(t, first, 20_000)

	session.Send(t, "now read internal/tokencount/counter.go")
	session.Send(t, compactCommandWithBudget("20k"))
	second := harness.LastInjectedTranscript(t, session)
	assertUnderBudget(t, second, 20_000)

	if !strings.Contains(second, "cap.go") {
		t.Fatal("the second compaction lost the first compaction's recovered content")
	}
}

func compactCommandWithBudget(budget string) string {
	return "/compact " + "--max-tokens " + budget
}

func assertUnderBudget(t *testing.T, transcript string, budget int) {
	t.Helper()
	counter := tokencount.LocalCounter(tokencount.FamilyClaude, "", tokencount.Settings{})
	if count := counter.Estimate(transcript); count >= budget {
		t.Fatalf("retained tokens = %d, want strictly under %d", count, budget)
	}
}
```

- [ ] **Step 2: Run the live test to verify it fails**

Run: `CGO_ENABLED=1 go test ./test/live/... -run TestLiveReorientCompactHonorsMaxTokens -count=1 -v`
Expected: FAIL before Task 5 and Task 6 land, because the sandbox daemon runs the build under test and the assertion names behavior this branch adds.

- [ ] **Step 3: Rewrite the overview page**

Rewrite the Tier 2 section of `docs/reorient/overview.md:13-19` to state the shipped behavior after this branch: clyde counts each message of the intercepted request, retains the newest messages that fit the budget, truncates the boundary message, summarizes everything older, and reads no transcript file. Delete the description of the message-count split, the byte cap, the 3,500-line disk render, and the older-half system-message deletion. State that `/compact` accepts the `clyde conversation export` arguments and that `--max-tokens` overrides `reorient_inject_max_tokens`.

Coordinate with the `worktree-reorient-compaction-class` branch on line 15, which describes detection. That branch corrects the same line to name the request class.

- [ ] **Step 4: Run the whole suite, then deploy and validate**

```bash
cd /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model
make check
make test
```

Open the pull request, merge it after review, then run `make deploy` from the primary checkout. The shared daemon is machine global, so confirm the running build SHA before trusting any live sample.

- [ ] **Step 5: Validate against a real compaction**

Run `/compact` with a 500k budget twice on a real interactive session, then confirm both compactions from the capture store and the saved transcript:

```bash
sqlite3 -readonly ~/.local/state/clyde/mitm/capture.db \
  "select r.id, datetime(r.ts/1000000000,'unixepoch'), substr(r.session_id,1,8), r.status
     from requests r
     where r.req_headers like '%Request-Class: compaction%'
     order by r.ts desc limit 5"
```

The request-class header comes from the `clyde-compaction-header` session's measurement of capture rows 363783 and 366501, and it is unverified here. Fall back to matching the prompt substring in the request body when the header query returns no rows.

Assert on the result: each saved `isCompactSummary` message counts under 500k tokens by the export counter, the second summary still includes the first compaction's recovered text, and `mitm.reorient_inject.split_planned` appears once per compaction with no `split_fallback` beside it.

- [ ] **Step 6: Commit**

```bash
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model add test/live docs/reorient/overview.md
git -C /Users/agoodkind/Sites/clyde-dev/clyde/.claude/worktrees/sentinel-preserve-model commit -S -m "$(cat <<'EOF'
Assert the compaction token budget live and document the token split

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Spec coverage

| Spec requirement | Task |
| --- | --- |
| Selection 1, messages stay in request form | 5 |
| Selection 2, the counter measures one message alone | 4, 5 |
| Selection 3 and 4, count from the newest and name the boundary message | 5 |
| Selection 5, strictly under the budget | 5 |
| Selection 6, the export counter and settings | 6 |
| Selection 7, message 0 is always summarized | 5 |
| Selection 8, no transcript file read | 5, 6 |
| Selection 9, forward unmodified and log a reason | 5 |
| Truncation 1 and 2, retain older messages plus the truncated boundary | 4 |
| Truncation 3 and 4, the synthetic error result | 4 |
| Arguments 1 through 3, read and parse the instruction block | 3 |
| Arguments 4, the instruction region stays unmodified | 4, 5 |
| Arguments 5, the budget argument and the configured default | 5 |
| Arguments 6, parsing never fails a compaction | 3, 5 |
| Arguments 7, one parameter declaration | 1, 2 |
| Package placement 1, Anthropic shapes in the adapter package | 4 |
| Package placement 2, prompt and arguments in the Claude package | 3 |
| Package placement 3, no Anthropic type in the hook package | 5 |
| Package placement 4, the counter stays generic | 6 |
| Removed list | 5, 6 |
