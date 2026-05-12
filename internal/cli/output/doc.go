// Package output is the universal CLI output-format primitive.
//
// Every subcommand that emits structured output routes its writes
// through an *Encoder constructed from the persistent --output-format
// flag. The flag is registered once on the root command via
// PersistentFlag; subcommands obtain a configured encoder via From.
//
// Contract: every subcommand emits exactly one JSON value per logical
// output unit. One-shot subcommands emit a single JSON value plus
// newline. Streaming subcommands emit one JSON value per line
// (newline delimited). Text output remains free-form for human
// readers and runs only when Format is FormatText.
//
// Type hygiene: the encoder carries no any-typed channel. Emit takes
// a closed Payload marker interface; every variant the CLI emits
// declares a zero-cost IsOutputPayload method next to the type and
// becomes a legal argument by virtue of that declaration. The marker
// interface is the closed substitute for any.
package output
