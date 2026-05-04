package codex

// This file used to host Codex-local regex strippers for synthetic content
// envelopes. Codex now delegates synthetic stripping to the render-owned
// fabric in [internal/adapter/render]. Use [adapterrender.StripSyntheticContent]
// from any Codex code path that needs to scrub synthetic UI envelopes from
// assistant content before reusing it on the next upstream request.
