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

Clyde reads a composer's messages by following the ordered list of message
references on the composer record, fetching each referenced message row in turn.

That list is known to be incomplete. Messages exist in the store that the record
does not reference, and they carry real conversation content, so a composer can
read short. Tracked as CLYDE-599.

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
