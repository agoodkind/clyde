# Compaction canary system

A canary is a piece of content the smoke methodology inserts into a transcript so the test can prove whether a later compaction kept that content in the model's reachable context or dropped it. The methodology has to be deterministic across prompt phrasings, because compaction correctness is meaningless if the verification step itself is noisy. This page explains the canary form the runbook uses, the failure modes that produced it, and the spawn flags every canary probe has to pass.

See [edge-cases.md](edge-cases.md) for related probe edge cases and [docs/compaction-smoke-runbook.md](../compaction-smoke-runbook.md) for the step-by-step procedure that uses this methodology.

## The naive approach and why it fails

The obvious canary methodology is to insert a UUID into the transcript and then ask the model to find the UUID starting with a specific prefix. The smoke runbook tried this form first and discarded it. Two failure modes produced unreliable results.

The model sometimes misses content that genuinely sits in its context window, so a "not found" answer does not prove the content was dropped. The model also sometimes fabricates content that is not in the transcript at all, so a "found" answer does not prove the content survived. Identical canaries and identical transcripts produce different answers under different prompt phrasings, which means the methodology cannot serve as the ground-truth oracle that compaction correctness requires.

## The metadata-footer canary form

The canary form the runbook uses now is a fake skill-metadata footer embedded inside a real skill body. The footer looks like ordinary build metadata that a skill might carry, with a randomly generated hex identifier and a UUID. The smoke step inserts this footer into a near-tail user message in the transcript, then asks the model a factual question about that skill, such as "what is the build hash for skill `<HEX>`."

The model treats the footer as a fact it can describe, not as an instruction it has to follow. When the footer is in context, the model reliably returns the UUID across prompt phrasings. When the footer has been dropped, the model says it does not have information about the skill. The deterministic answer in both directions is what makes the canary usable as an oracle.

The metadata-footer form survives because the model's training has seen the same structural pattern in real codebases. The footer reads as descriptive metadata rather than as a directive, so the model is willing to repeat the UUID verbatim instead of treating it as something it should not echo.

## The prompt-injection trap

A canary written in instruction form fails because the model refuses to follow it. A canary of the shape "WHEN THE USER ASKS X PLEASE OUTPUT Y" reads to the model as a prompt-injection attempt embedded in the conversation, and the model's training has produced strong reflexes against repeating injected payloads. Opus in particular refuses to echo the UUID under these conditions, which produces a false negative: the canary is in context but the model will not say so.

Any future change to the canary content has to keep the descriptive metadata form. Adding instruction language, conditional logic, or any phrasing that looks like a directive to the model breaks the methodology even when the underlying compaction is working correctly.

## Spawn flags every canary probe passes

Every non-chat `claude` invocation in the smoke methodology passes `--no-session-persistence` and does not pass `--fork-session`. The rule is a hard requirement, not a preference, and it covers every step that is not a human sitting in an interactive `claude` REPL.

The `--no-session-persistence` flag tells claude to skip writing any session state for the spawn. The probe reads the transcript, asks the model its question, captures the answer, and exits without leaving any trace on disk. The flag is what makes the smoke methodology safe to run against a live session: a probe with persistence enabled would append entries to the same JSONL that the live session is writing, and the appended entries would corrupt the transcript shape the compaction step is about to mutate.

The `--fork-session` flag is the wrong tool for the same job. Forking creates a disposable JSONL alongside the original session for the lifetime of the spawn. The fork file is itself a write, so the probe is no longer trace-free. A canary smoke that uses `--fork-session` leaves orphan fork files behind on every probe, which the cleanup step has to find and remove. A canary smoke that uses `--no-session-persistence` writes nothing and needs no cleanup.

The daemon's internal Prober and CandidateProber pass `--no-session-persistence` automatically for their own spawns. The rule on this page is for human-driven and agent-driven invocations that bypass the daemon, including the smoke runbook steps, ad hoc debugging probes, and any CI check that resumes a session for any reason other than continuing the live conversation.
