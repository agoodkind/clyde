package codex

import adapteropenai "goodkind.io/clyde/internal/adapter/openai"

// SSEWriter is the narrow contract the root-owned SSE writer
// implements so the codex Provider can emit chunks without depending
// on the daemon's HTTP plumbing. The provider construction wraps
// this interface in a provider.EventWriter the dispatcher hands to
// Provider.Execute.
type SSEWriter interface {
	WriteSSEHeaders()
	EmitStreamChunk(string, adapteropenai.StreamChunk) error
	WriteStreamDone() error
}
