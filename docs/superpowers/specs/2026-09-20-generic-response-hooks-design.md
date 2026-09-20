# Generic response hooks

Clyde needs to evaluate intercepted model responses without placing provider wire types in the policy implementation.

## Boundary

The generic hook package defines an action interface and a provider response adapter interface. An action receives plain assistant text and returns optional model feedback. A provider response adapter decides whether a request uses its response format, extracts assistant text, and appends feedback in the same response format.

Provider packages register response adapters during startup. The generic package selects an adapter by the typed MITM provider identifier. The generic package does not import Anthropic or OpenAI response types.

Agent Gate implements one action through a provider independent command. The command accepts a neutral response event and returns its diagnostic. Agent Gate does not receive a Claude, Codex, Anthropic, or OpenAI hook payload.

## Request selection

The response hook rejects compaction requests before reading the response body. Each provider adapter accepts only ordinary response routes in its own package. Reorientation remains the only hook that handles compaction responses.

## Provider implementations

The Claude MITM provider registers an Anthropic Messages adapter. The adapter extracts completed assistant text from Anthropic event streams and appends a text block before the terminal events.

The Codex MITM provider registers an OpenAI Responses adapter. The adapter extracts completed assistant output from Responses event streams and appends a native output text item before the terminal response event.

## Failure behavior

An unavailable checker or invalid provider response returns the original response and records an error. A rule violation appends the diagnostic and a direct retry instruction. A compliant response remains byte identical.

## Verification

Agent Gate tests the neutral response event through its real daemon boundary. Clyde tests both provider adapters with complete event streams. Clyde also tests the generic action chain through the live MITM proxy and confirms that compaction responses remain unchanged.
