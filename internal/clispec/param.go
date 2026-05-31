package clispec

import (
	"slices"
	"strings"
)

// ParamKind is the closed set of value shapes a parameter can carry. The kind
// drives both the mcp schema property constructor and the cobra flag
// constructor.
type ParamKind uint8

const (
	// KindString is free text.
	KindString ParamKind = iota
	// KindInt is a whole number.
	KindInt
	// KindBool is an on/off switch.
	KindBool
	// KindEnum is a string constrained to Values. The terminal rejects a
	// value outside Values; the MCP tool falls back to the default.
	KindEnum
)

// Param binds one named input into the per-operation input struct I. It is
// fully typed: exactly one of the three bind closures is non-nil, matching
// Kind, and it writes the decoded value into a named field of *I. There is no
// any-typed channel and no reflection.
type Param[I Input] struct {
	// Canonical is the snake_case input name. The terminal flag is the
	// dash-spelled form; the MCP property uses Canonical verbatim.
	Canonical   string
	Kind        ParamKind
	Required    bool
	Description string
	// CLIOnly keeps a parameter off the MCP surface. The export file path
	// flag uses this: the terminal writes a file, the MCP tool returns text.
	CLIOnly bool
	// Values lists the accepted words for KindEnum. It is empty otherwise.
	Values []string

	DefaultStr  string
	DefaultInt  int
	DefaultBool bool

	bindString func(in *I, v string)
	bindInt    func(in *I, v int)
	bindBool   func(in *I, v bool)
}

// flagName returns the dash-spelled terminal flag for this parameter.
func (p Param[I]) flagName() string {
	return strings.ReplaceAll(p.Canonical, "_", "-")
}

// StringParam declares a free-text input.
func StringParam[I Input](canonical, description, def string, required bool, set func(*I, string)) Param[I] {
	return Param[I]{
		Canonical:   canonical,
		Kind:        KindString,
		Required:    required,
		Description: description,
		CLIOnly:     false,
		Values:      nil,
		DefaultStr:  def,
		DefaultInt:  0,
		DefaultBool: false,
		bindString:  set,
		bindInt:     nil,
		bindBool:    nil,
	}
}

// IntParam declares a whole-number input.
func IntParam[I Input](canonical, description string, def int, set func(*I, int)) Param[I] {
	return Param[I]{
		Canonical:   canonical,
		Kind:        KindInt,
		Required:    false,
		Description: description,
		CLIOnly:     false,
		Values:      nil,
		DefaultStr:  "",
		DefaultInt:  def,
		DefaultBool: false,
		bindString:  nil,
		bindInt:     set,
		bindBool:    nil,
	}
}

// BoolParam declares an on/off input.
func BoolParam[I Input](canonical, description string, def bool, set func(*I, bool)) Param[I] {
	return Param[I]{
		Canonical:   canonical,
		Kind:        KindBool,
		Required:    false,
		Description: description,
		CLIOnly:     false,
		Values:      nil,
		DefaultStr:  "",
		DefaultInt:  0,
		DefaultBool: def,
		bindString:  nil,
		bindInt:     nil,
		bindBool:    set,
	}
}

// EnumParam declares a string constrained to values. The terminal rejects an
// unknown word; the MCP tool falls back to def. Both front ends share values.
func EnumParam[I Input](canonical, description, def string, values []string, set func(*I, string)) Param[I] {
	return Param[I]{
		Canonical:   canonical,
		Kind:        KindEnum,
		Required:    false,
		Description: description,
		CLIOnly:     false,
		Values:      values,
		DefaultStr:  def,
		DefaultInt:  0,
		DefaultBool: false,
		bindString:  set,
		bindInt:     nil,
		bindBool:    nil,
	}
}

// Arg is a terminal positional input that maps to a named MCP input. The
// terminal reads it as a bare word in declaration order; the MCP tool reads it
// as a named property under MCPName. Every Arg in this package is required.
type Arg[I Input] struct {
	MCPName     string
	Description string
	bind        func(in *I, v string)
}

// PositionalArg declares a required positional input.
func PositionalArg[I Input](mcpName, description string, set func(*I, string)) Arg[I] {
	return Arg[I]{
		MCPName:     mcpName,
		Description: description,
		bind:        set,
	}
}

// placeholder returns the upper-case token shown in terminal usage, e.g.
// CONVERSATION_ID.
func (a Arg[I]) placeholder() string {
	return strings.ToUpper(a.MCPName)
}

func enumContains(values []string, candidate string) bool {
	return slices.Contains(values, candidate)
}
