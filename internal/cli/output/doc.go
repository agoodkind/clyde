// Package output is the universal CLI output-format primitive.
//
// The persistent --output-format flag is registered once on the root command
// via PersistentFlag, so every subcommand inherits it. A subcommand reads the
// resolved value with ParseFormat and chooses how to render its reply: text is
// free-form for human readers, json emits a structured document. For
// daemon-owned queries the daemon performs the rendering and the subcommand
// prints the returned string.
package output
