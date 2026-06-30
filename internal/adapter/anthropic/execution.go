package anthropic

import adapterresolver "goodkind.io/clyde/internal/adapter/resolver"

// JSONCoercion carries the optional structured-output post-processing
// contract used by the current OpenAI facade collect path. Native
// Anthropic ingress can leave both hooks unset.
type JSONCoercion struct {
	Coerce   func(text string) string
	Validate func(text string) bool
}

// PreparedRequest is the provider-owned Anthropic execution input.
// OpenAI ingress prepares one from a ResolvedRequest today; future
// native Anthropic ingress can construct it directly from `/v1/messages`.
type PreparedRequest struct {
	Request       Request
	Resolved      *adapterresolver.ResolvedRequest
	RequestID     string
	TrackerKey    string
	JSONCoercion  JSONCoercion
	IncludeUsage  bool
	Stream        bool
	NativeIngress bool
}
