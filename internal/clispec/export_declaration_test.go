package clispec

import (
	"slices"
	"testing"

	"goodkind.io/clyde/internal/conversation/exportargs"
)

func paramKindFor(kind exportargs.Kind) ParamKind {
	switch kind {
	case exportargs.KindString:
		return KindString
	case exportargs.KindInt:
		return KindInt
	case exportargs.KindBool:
		return KindBool
	case exportargs.KindEnum:
		return KindEnum
	case exportargs.KindEnumList:
		return KindEnumList
	default:
		return KindFloat
	}
}

// These two flags select a destination file. internal/conversation/exportargs
// declares export content, and omits them.
var exportDestinationFlags = map[string]bool{"output": true, "stdout": true}

// TestExportParamsMatchDeclarations fails when the terminal export flags and
// internal/conversation/exportargs disagree on which arguments exist or on any
// argument's kind, wording, default, or allowed values. It also fails when a
// rendered parameter omits its bind closure, which registerFlag calls without a
// nil check.
func TestExportParamsMatchDeclarations(t *testing.T) {
	t.Parallel()
	declarations := map[string]exportargs.Declaration{}
	for _, declaration := range exportargs.Declarations() {
		declarations[declaration.Canonical] = declaration
	}

	rendered := map[string]bool{}
	for _, param := range exportParams() {
		rendered[param.Canonical] = true
		if exportDestinationFlags[param.Canonical] {
			continue
		}
		declaration, ok := declarations[param.Canonical]
		if !ok {
			t.Errorf("terminal flag %q is not declared in exportargs", param.Canonical)
			continue
		}
		if param.Kind != paramKindFor(declaration.Kind) {
			t.Errorf("argument %q kind = %v, want %v", param.Canonical, param.Kind, paramKindFor(declaration.Kind))
		}
		if param.Required != declaration.Required {
			t.Errorf("argument %q required = %t, want %t", param.Canonical, param.Required, declaration.Required)
		}
		if param.CLIOnly != declaration.CLIOnly {
			t.Errorf("argument %q cli-only = %t, want %t", param.Canonical, param.CLIOnly, declaration.CLIOnly)
		}
		if param.Description != declaration.Description {
			t.Errorf("argument %q description = %q, want %q", param.Canonical, param.Description, declaration.Description)
		}
		if !slices.Equal(param.Values, declaration.Values) {
			t.Errorf("argument %q values = %v, want %v", param.Canonical, param.Values, declaration.Values)
		}
		if param.DefaultStr != declaration.DefaultStr {
			t.Errorf("argument %q string default = %q, want %q", param.Canonical, param.DefaultStr, declaration.DefaultStr)
		}
		if param.DefaultInt != declaration.DefaultInt {
			t.Errorf("argument %q int default = %d, want %d", param.Canonical, param.DefaultInt, declaration.DefaultInt)
		}
		if param.DefaultBool != declaration.DefaultBool {
			t.Errorf("argument %q bool default = %t, want %t", param.Canonical, param.DefaultBool, declaration.DefaultBool)
		}
		if !exportParamStores(param) {
			t.Errorf("argument %q has no bind closure for kind %v", param.Canonical, param.Kind)
		}
	}

	for canonical := range declarations {
		if !rendered[canonical] {
			t.Errorf("declared argument %q has no terminal flag", canonical)
		}
	}
}

func exportParamStores(param Param[exportInput]) bool {
	switch param.Kind {
	case KindString, KindEnum:
		return param.bindString != nil
	case KindInt:
		return param.bindInt != nil
	case KindBool:
		return param.bindBool != nil
	case KindFloat:
		return param.bindFloat != nil
	case KindStringList, KindEnumList:
		return param.bindStrSlice != nil
	default:
		return false
	}
}
