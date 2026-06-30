package output

import "fmt"

// Format names the output mode a subcommand renders in. The zero
// value is FormatText so a caller that forgets to set the field still
// behaves the same as the default human-readable mode.
type Format string

const (
	// FormatText is the default human-readable mode. Subcommands
	// supply a text renderer that runs only when the encoder is
	// configured for FormatText.
	FormatText Format = "text"
	// FormatJSON emits structured JSON. One-shot subcommands write
	// a single JSON value plus newline; streaming subcommands write
	// one JSON value per line.
	FormatJSON Format = "json"
)

// FlagName is the persistent flag string registered on the root
// command. Exported so tests and helper packages can reference the
// same literal without hard-coding it.
const FlagName = "output-format"

// UnknownFormatError is returned by ParseFormat when the input does
// not match a known Format. The error message lists the supported
// values so the caller can surface them to the user without
// duplicating the set.
type UnknownFormatError struct {
	Input string
}

// Error reports the unsupported format value alongside the supported
// set so the caller can surface a complete message without
// duplicating the enumeration.
func (e *UnknownFormatError) Error() string {
	return fmt.Sprintf("output: unknown format %q (supported: %s, %s)",
		e.Input, FormatText, FormatJSON)
}

// ParseFormat resolves a user-supplied string into a Format. The
// empty string maps to FormatText so an unset flag behaves like the
// default. Unknown values produce an *UnknownFormatError.
func ParseFormat(s string) (Format, error) {
	switch s {
	case "", string(FormatText):
		return FormatText, nil
	case string(FormatJSON):
		return FormatJSON, nil
	default:
		return "", &UnknownFormatError{Input: s}
	}
}
