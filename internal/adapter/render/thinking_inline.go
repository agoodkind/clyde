package render

import "strings"

func ThinkingInlineOpen() string {
	return "<!--clyde-thinking-->\n> **Thinking...**\n>\n"
}

func FormatThinkingInlineDelta(open bool, text string) string {
	if !open {
		return strings.ReplaceAll(text, "\n", "\n> ")
	}
	return ThinkingInlineOpen() + "> " + strings.ReplaceAll(text, "\n", "\n> ")
}

func ThinkingInlineClose() string {
	return "\n<!--/clyde-thinking-->\n\n"
}
