package clispec

import (
	"fmt"

	conv "goodkind.io/clyde/internal/conversation"
)

func recordWhitespaceSelection(in *exportInput, mode conv.WhitespaceMode, selected bool) {
	if selected {
		in.WhitespaceSelections = append(in.WhitespaceSelections, mode)
	}
}

func resolveExportWhitespace(in exportInput) (conv.WhitespaceMode, error) {
	if len(in.WhitespaceSelections) > 1 {
		return "", fmt.Errorf("specify only one whitespace selector; use one of --whitespace, --preserve, --tidy, --compact, or --dense")
	}
	if len(in.WhitespaceSelections) == 1 {
		return in.WhitespaceSelections[0], nil
	}
	return in.Options.Whitespace, nil
}
