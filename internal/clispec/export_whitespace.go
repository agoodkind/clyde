package clispec

import (
	"fmt"

	conv "goodkind.io/clyde/internal/conversation"
)

func recordWhitespaceSelection(in *exportInput, mode conv.WhitespaceMode) {
	if mode != "" {
		in.WhitespaceSelections = append(in.WhitespaceSelections, mode)
	}
}

func resolveExportWhitespace(in exportInput) (conv.WhitespaceMode, error) {
	if len(in.WhitespaceSelections) > 1 {
		return "", fmt.Errorf("specify only one whitespace selector; use one of --whitespace, --preserve, --tidy, or --dense")
	}
	if len(in.WhitespaceSelections) == 1 {
		return in.WhitespaceSelections[0], nil
	}
	if in.Options.Whitespace == "" {
		return conv.WhitespacePreserve, nil
	}
	return in.Options.Whitespace, nil
}
