package provider

import adapterrender "goodkind.io/clyde/internal/adapter/render"

// EventWriter is the only path a Provider has into the adapter-owned
// render and OpenAI framing pipeline. Providers emit normalized
// render.Event values only; adapter-owned writers decide how those
// events become streamed chunks or collected responses.
type EventWriter interface {
	// WriteEvent serializes one normalized event onto the wire.
	// Implementations may convert the event into streamed or buffered
	// OpenAI-shaped chunks internally before writing.
	WriteEvent(ev adapterrender.Event) error
	// Flush forces buffered output to the network. Providers call
	// it at well-defined boundaries (response.created,
	// response.completed) so clients see incremental progress.
	Flush() error
}
