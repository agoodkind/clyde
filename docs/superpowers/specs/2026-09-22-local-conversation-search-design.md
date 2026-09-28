# Local conversation search

This historical design is superseded by [Embedded conversation search](2026-09-27-embedded-search-design.md). Its backend switch, row replacement, deletion, and fixed depth search instructions are not implementation requirements.

Clyde needs cross-provider conversation search without LMS, Docker, or an external model provider. A local backend stores a compact index on disk and loads it into memory for search on computers with 16 or 24 GB of RAM. Local ranking may be less precise than LMS ranking.

## Select one backend

Add an explicit `backend = "local"` or `backend = "lms"` setting under `[conversation.semantic]`. Keep `ingestion_enabled` and `search_enabled` as independent switches. Require a backend choice when either operation is enabled. Both operations use that choice.

When LMS is selected and unavailable, return the existing typed unavailable error. Do not search the local index or change the configured backend. Raw conversation listing, reading, context, and export remain available.

## Reuse conversation ingestion

Use Clyde's existing provider readers, raw conversation index, refresh scheduling, load options, content selection, and `BuildSemanticConversationDocuments` projection for both backends. Do not create another provider parser, transcript scan, or conversation ingestion path. Preserve conversation IDs, message indexes, metadata, and the selected content kinds.

The projection omits a message only when all selected content is empty. It retains tool-only and selected reasoning-only messages. Empty or whitespace-only text produces no searchable text passage, while selected tool and reasoning content still produces passages. Historical empty rows in the LMS collection do not define the local index contents.

Filtering and row preparation are generic Clyde work before backend dispatch. Clyde applies the selected content policy, omits empty fields, and converts each projected message into text, tool, and thinking rows with stable keys and metadata. The selected backend receives those eligible rows. It does not decide which provider messages or content kinds qualify.

CLYDE-629 already specifies the Clyde cutover to LMS's generic ingestion RPC after LMS-15, LMS-16, and LMS-17. Reuse that row preparation and ingestion boundary for the local backend. Preserve LMS row keys, fingerprints, and collection reconciliation during its cutover. The local backend records conversation fingerprints and projection changes in its own persisted index. A local rebuild reads Clyde's raw conversation records through the same preparation path; it does not import LMS vectors or rows.

The backend may subdivide each eligible row for its model's input limit. Split every long field into ordered passages with overlap, and index every passage without truncation. Retain the conversation ID, original message index, content kind, and passage position so a match can resolve to the existing transcript window. Model token limits and vector encoding do not change Clyde's content policy.

## Share search behavior

Use the existing `conversationSearchSource` lookup boundary for both backends. Clyde defines request validation, filter meaning, result visibility, record hydration, pagination, facets, and typed errors once. CLYDE-643 specifies Clyde's cutover to the generic LMS search RPC after LMS-18.

Search results are deterministic. The same query, filters, and corpus return the same ordered results. Every page is a slice of one ranking, and a smaller limit returns a prefix of a larger limit's results.

Clyde prepares the complete candidate restriction before dispatch. It resolves provider, workspace, conversation, and archive scope against one snapshot of its raw index and produces the allowed conversation IDs. Hidden subagent conversations, archived conversations the request excludes, and conversations missing from the raw index are absent from that set. Clyde sends the allowed set with the role, time, and message-index restrictions. An empty allowed set returns an empty result without a backend call.

The backend applies every restriction during candidate selection. It computes one ranking per query at a fixed depth that does not depend on the limit, offset, or per-conversation cap. It orders equal scores by row key. It walks that ranking once to apply the minimum score, the per-conversation cap, and the requested window. The backend never splits one request into separately ranked searches. It does not interpret Clyde provider formats or decide filter policy.

Clyde hydrates hits from the snapshot that produced the allowed set. Clyde never drops ranked hits after retrieval or retries a query with a larger limit. The local backend scores every allowed passage in RAM and ranks exactly. LMS ranks within the Milvus 16,384-row search ceiling, and a page beyond that depth is the end of the results. Both backends return the same public result shape. Backend selection never depends on whether a query succeeds or returns matches.

## Bound local storage and memory

A sampled LMS collection contained 3,525,870 rows. About one quarter of sampled rows had empty stored content from older indexing behavior. Excluding those rows and splitting nonempty content for a small model gives an approximate 7.6 to 8.6 million local passages. A 384-dimension binary vector plus packed passage ID uses about 390 to 444 MiB for that estimate. The estimate assumes 384 dimensions and one bit per dimension. It excludes the model, lookup structures, persisted text references, and runtime overhead; it is not a measured local index size.

Bundle a small model that runs without a model provider. Persist the compact index after ingestion and load its search data into RAM at daemon startup. Bound ingestion batches and peak build memory so indexing and search remain usable on 16 GB and 24 GB machines. Measure actual index size, startup time, peak and steady memory, update cost, and query latency on the corpus before setting final budgets.

## Verify the contract

Verify the same cross-provider search request through the CLI and MCP with each selected backend. Verify tool-only messages, empty text, long passage coverage, filters, pagination, incremental updates, restart from the saved index, and full rebuild from Clyde's raw index. Verify that repeated requests return the same order, that a smaller limit returns a prefix of a larger one, and that a visible match ranked below many hidden or archived matches still appears. Verify that an unavailable selected LMS returns an error without local fallback. Report actual disk, memory, startup, and query measurements separately from the corpus estimate.
