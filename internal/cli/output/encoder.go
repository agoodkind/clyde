package output

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// Payload is the closed set of typed values the CLI output encoder
// can emit. The IsOutputPayload method is a marker that types
// implement to opt into being emitted by Encoder. Every CLI emit
// value is a concrete Go type that implements this interface; there
// is no any-typed channel anywhere in the output pipeline.
//
// New variants are added by declaring IsOutputPayload next to the
// variant type definition in its owning package, not by widening this
// interface or by adding a method here.
type Payload interface {
	IsOutputPayload()
}

// Encoder routes a typed Payload through either a JSON writer or a
// caller-supplied text renderer based on Format. Subcommands
// construct an Encoder once per RunE via From and then call Emit (or
// a typed wrapper that calls through to Emit) for each logical
// output unit.
type Encoder struct {
	Format Format
	W      io.Writer
}

// Emit writes one logical output unit. In FormatJSON mode the
// payload is encoded as a single JSON value plus newline (the
// streaming and one-shot contracts share this same shape, so a
// streaming caller invokes Emit once per record). In FormatText mode
// the encoder delegates to textFn when non-nil; a nil textFn in text
// mode is a no-op so a subcommand can declare a JSON-only payload
// without inventing a text renderer.
func (e *Encoder) Emit(p Payload, textFn func(io.Writer) error) error {
	switch e.Format {
	case FormatJSON:
		if err := json.NewEncoder(e.W).Encode(p); err != nil {
			slog.Warn("output.encoder.encode_failed",
				"component", "cli",
				"subcomponent", "output",
				"err", err,
			)
			return fmt.Errorf("output: encode: %w", err)
		}
		return nil
	case FormatText:
		if textFn == nil {
			return nil
		}
		return textFn(e.W)
	default:
		return fmt.Errorf("output: unknown format %q", e.Format)
	}
}
