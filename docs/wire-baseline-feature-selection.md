# Feature-aware wire flavor selection

How clyde chooses which learned claude-cli identity to replay on an Anthropic request, so the outbound beta header is always the one claude-cli itself would send for that model on this account.

## The contract

Clyde never decides which betas a model gets. claude-cli decides, and clyde mirrors it. The user manages nothing. There is no beta config.

## Why selection has to know the model

The learned baseline holds one flavor per observed claude-cli wire shape. Flavors already differ by beta set, because claude-cli sends different betas for different models. The gap is selection: today the egress picks one interactive flavor and replays it on every request, ignoring the model it is actually serving.

That misapplies model-specific betas. `context-1m-2025-08-07` is the clear case. claude-cli sends it for the models whose 1M window the account includes and omits it elsewhere. Observed in `mitm/capture.db` on 2026-06-09: present on 680/682 Fable requests and 65/67 Opus, absent on all 48 Haiku. The beta tracks the model, not the request. When clyde replays a Fable flavor onto a Sonnet request, Sonnet inherits a 1M beta the account does not grant for Sonnet, and Anthropic rejects it with `rate_limit_error: "Usage credits are required for long context requests"`.

## How selection should work

Each learned flavor carries the feature vector it was captured with: the model, the context tier, the thinking mode, whether structured output was set, whether tools were present. At request time the egress builds the same vector from the resolved request and replays the flavor that matches it. The beta header is then correct by construction, because it is the exact set claude-cli sent for that combination.

A model the proxy has never seen has no flavor to replay. The egress fails loud with an HTTP 503 that names the model and says to run claude-cli once with it through the MITM. Seeding is the only manual act, and it is a normal claude session, not a config edit.

## The one deliberate deviation

Clyde shows reasoning in Cursor, so it always drops the two thinking-redaction betas claude-cli carries (`thinking-token-count-2026-05-13`, `redact-thinking-2026-02-12`). This is a fixed property of the adapter, not a setting. It is the single point where clyde does not mirror claude-cli, and it is the same for every model.

## What this removes

The `per_context_betas` and `beta_suppress` knobs go away. Both existed to patch the wrong-flavor problem by hand. Once selection is feature-aware, the learned flavor is already right, and there is nothing left to override.
