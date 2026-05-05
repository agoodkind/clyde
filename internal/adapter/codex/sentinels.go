package codex

// This file used to host Codex-local regex strippers for synthetic content
// envelopes. Codex now delegates synthetic parsing to the render-owned
// fabric in [internal/adapter/render]. Codex code paths that need to handle
// synthetic UI envelopes from assistant content call
// [adapterrender.ExtractSyntheticParts] and decide per kind how to materialize
// each part on the next upstream request.
