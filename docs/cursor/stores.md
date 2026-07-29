# Cursor stores

Cursor keeps conversations in three places, and Clyde reads all three. Which
store holds a conversation determines its native id, its artifact kind, and how
Clyde reads its messages.

This page covers conversation reading. For proxy capture see
[Cursor capture](capture.md), and for the OpenAI-compatible ingress and model
routing see [Cursor](../cursor.md).

## The three stores

**Agent transcripts** are JSONL files, one per conversation, under the Cursor
projects directory. Clyde indexes a file whose name matches its containing
directory, which is how a conversation is told apart from the subagent
transcripts stored beside it. The artifact kind is `cursor_agent_transcript` and
the native id is the conversation uuid.

**Composers** live in Cursor's global storage database. A composer record holds
the conversation's metadata, and its messages are separate rows keyed by
composer and message id. The artifact kind is `cursor_composer`, or
`cursor_background_composer` when Cursor ran the conversation in the background.
The native id is the composer id.

**Legacy chats** live in a per-workspace database, one row per workspace holding
every chat tab. The artifact kind is `cursor_legacy_chat` and the native id
joins the workspace hash and the tab id, because a tab id is unique only inside
its workspace.

## Reading a composer

Clyde reads every message row stored under a composer, not only the ones its
record references. The record's ordered list of message references is not a
complete index: messages exist in the store that it never names, and they carry
real conversation content, so following the list alone reads a composer short.

Reading everything needs its own judgement, because most unreferenced rows are
superseded copies. Cursor rewrites a turn under a new message id and leaves the
previous row in place, marking both with the same server-side message identity,
so that identity is what separates a copy from a turn that genuinely happened
twice. The record's order is reproduced exactly for the messages it does name,
and a recovered message is placed by its write time between the two dated
messages that bracket it.

A composer with no message references at all is still a conversation when its
message rows hold content, and Clyde indexes it on that basis rather than on the
reference list. A composer with no stored messages is a draft and stays out.

## Subagents

Cursor writes a subagent's transcript one directory deeper than its parent's, so
the filename no longer matches its containing directory and Clyde does not index
it as a conversation.

The global store marks a composer as a subagent on its own record. Clyde does not
read that marker today, so a subagent composer is indexed like any other.

## Access

Cursor is usually running while Clyde reads, so Clyde opens every Cursor
database read-only and never writes to one. Clyde does not delete, prune, or
migrate anything Cursor owns.
