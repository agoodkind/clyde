package vectorsearch

import "strings"

// sourceMatchContextBlocks records a match found only after removing leading
// context blocks from a user text row.
const sourceMatchContextBlocks = "context_blocks"

const contextBlockSpace = " \t\r\n"

// cursorContextTags lists the context block tags Cursor writes before typed user text.
var cursorContextTags = []string{
	"attached_files",
	"cursor_commands",
	"external_links",
	"git_diff_from_branch_to_main",
	"manually_attached_skills",
	"uploaded_documents",
}

// stripLeadingContextBlocks removes complete leading context blocks and returns the
// remaining user text. It returns false if the text does not start with a listed tag,
// a block lacks a closing tag, or only whitespace remains after the blocks.
func stripLeadingContextBlocks(text string) (string, bool) {
	rest := strings.TrimLeft(text, contextBlockSpace)
	stripped := false
	for {
		name, found := leadingContextTag(rest)
		if !found {
			break
		}
		closing := "</" + name + ">"
		end := strings.Index(rest, closing)
		if end < 0 {
			return text, false
		}
		rest = strings.TrimLeft(rest[end+len(closing):], contextBlockSpace)
		stripped = true
	}
	if !stripped || strings.TrimSpace(rest) == "" {
		return text, false
	}
	return rest, true
}

// leadingContextTag returns the listed context block tag at the start of the text.
func leadingContextTag(text string) (string, bool) {
	for _, name := range cursorContextTags {
		if strings.HasPrefix(text, "<"+name+">") {
			return name, true
		}
	}
	return "", false
}
