package vectorsearch

import (
	"slices"
	"strconv"
	"strings"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	// sourceMatchStoredRows indicates that a blank text family's stored tool and thinking
	// rows and nearest stored text rows match the source.
	sourceMatchStoredRows = "stored_rows"
	familyMessagePart     = 2
)

// blankFamilyIndex indexes stored families and their message positions for one conversation.
type blankFamilyIndex struct {
	families map[string][]collection.StoredRow
	// texts lists the positions of stored text families that contain text, in order.
	texts []int
	// textKeys maps each position in texts to its family key.
	textKeys map[int]string
	// siblings maps a position to its stored tool and thinking family keys.
	siblings map[int][]string
}

// bindBlankFamilies records the source date of each blank text family that passes
// verifyBlankFamily.
func (lookup sourceTimeLookup) bindBlankFamilies(families map[string][]collection.StoredRow) {
	if len(lookup.documents) == 0 {
		return
	}
	index := newBlankFamilyIndex(families)
	for key, members := range families {
		familyKind, _, _ := strings.Cut(key, familyPartSeparator)
		message, parsed := familyMessage(key)
		if familyKind != conversationTextFamily || !parsed || !familyIsBlank(members) {
			continue
		}
		if stamp, verified := lookup.verifyBlankFamily(index, message, rowRole(members)); verified {
			lookup.blankBindings[key] = stamp
		}
	}
}

func newBlankFamilyIndex(families map[string][]collection.StoredRow) blankFamilyIndex {
	index := blankFamilyIndex{families: families, texts: nil, textKeys: map[int]string{}, siblings: map[int][]string{}}
	for key, members := range families {
		message, parsed := familyMessage(key)
		if !parsed {
			continue
		}
		familyKind, _, _ := strings.Cut(key, familyPartSeparator)
		switch {
		case familyKind != conversationTextFamily:
			index.siblings[message] = append(index.siblings[message], key)
		case !familyIsBlank(members):
			index.texts = append(index.texts, message)
			index.textKeys[message] = key
		}
	}
	slices.Sort(index.texts)
	return index
}

// verifyBlankFamily returns a source message's date only when all checks pass:
//   - The source message exists at the blank family's position, has blank text,
//     matches the stored role regardless of letter case, and has a known date.
//   - The source generates rows for that message.
//   - Every stored tool and thinking family matches the generated text for each
//     stored path.
//   - At least one stored tool or thinking row has a known date, and every such
//     row with a known date has the source message's date.
//   - A stored text family containing text exists on each side. The source
//     generates rows at both positions, and each family matches that generated text.
//   - Known dates in the nearest preceding text family do not exceed the source
//     date. Known dates in the nearest following text family do not precede it.
func (lookup sourceTimeLookup) verifyBlankFamily(index blankFamilyIndex, message int, role string) (int64, bool) {
	doc, found := lookup.documents[message]
	if !found || strings.TrimSpace(doc.Text) != "" || !strings.EqualFold(doc.Role, role) || doc.TimestampUnix == missingTimestampUnix {
		return 0, false
	}
	generated, generatedOK := generatedRowText(doc)
	if !generatedOK {
		return 0, false
	}
	datedSibling := false
	for _, key := range index.siblings[message] {
		members := index.families[key]
		if !rowsEqualGenerated(members, generated) {
			return 0, false
		}
		for _, row := range members {
			stamp, dated := rowStoredDate(row)
			if dated && stamp != doc.TimestampUnix {
				return 0, false
			}
			datedSibling = datedSibling || dated
		}
	}
	if !datedSibling {
		return 0, false
	}
	position, _ := slices.BinarySearch(index.texts, message)
	if position == 0 || position == len(index.texts) {
		return 0, false
	}
	previous, next := index.texts[position-1], index.texts[position]
	if !lookup.textRowsExact(index, previous, func(stamp int64) bool { return stamp <= doc.TimestampUnix }) {
		return 0, false
	}
	if !lookup.textRowsExact(index, next, func(stamp int64) bool { return stamp >= doc.TimestampUnix }) {
		return 0, false
	}
	return doc.TimestampUnix, true
}

// textRowsExact reports whether each stored text path at message matches the generated
// source text and every known stored date passes the ordered check.
func (lookup sourceTimeLookup) textRowsExact(index blankFamilyIndex, message int, ordered func(int64) bool) bool {
	doc, found := lookup.documents[message]
	if !found {
		return false
	}
	generated, generatedOK := generatedRowText(doc)
	if !generatedOK {
		return false
	}
	members := index.families[index.textKeys[message]]
	if !rowsEqualGenerated(members, generated) {
		return false
	}
	for _, row := range members {
		if stamp, dated := rowStoredDate(row); dated && !ordered(stamp) {
			return false
		}
	}
	return true
}

// generatedRowText generates rows from doc and returns their text by relative path.
func generatedRowText(doc semsearch.SemDoc) (map[string]string, bool) {
	chunks, err := documentChunks(doc, conversationChunkMaxBytes)
	if err != nil {
		return nil, false
	}
	generated := make(map[string]string, len(chunks))
	for _, chunk := range chunks {
		generated[chunk.RelativePath] = chunk.Content
	}
	return generated, true
}

// rowsEqualGenerated reports whether each stored path has complete text that equals
// the generated text for that path.
func rowsEqualGenerated(members []collection.StoredRow, generated map[string]string) bool {
	byPath := make(map[string][]collection.StoredRow, len(members))
	for _, row := range members {
		byPath[row.RelativePath] = append(byPath[row.RelativePath], row)
	}
	for path, rows := range byPath {
		text, complete := pathText(rows)
		want, found := generated[path]
		if !complete || !found || text != want {
			return false
		}
	}
	return true
}

func familyIsBlank(members []collection.StoredRow) bool {
	for _, row := range members {
		if strings.TrimSpace(row.Content) != "" {
			return false
		}
	}
	return true
}

// familyMessage returns the message position in a family key.
func familyMessage(key string) (int, bool) {
	parts := strings.Split(key, familyPartSeparator)
	if len(parts) <= familyMessagePart {
		return 0, false
	}
	message, err := strconv.Atoi(parts[familyMessagePart])
	return message, err == nil
}

// rowStoredDate returns a stored timestamp other than NULL and the unknown time marker.
func rowStoredDate(row collection.StoredRow) (int64, bool) {
	cell, found := row.Scalars[timestampUnixColumn]
	if !found || cell.State != collection.ScalarCellValue || cell.Value.Int64 == missingTimestampUnix {
		return 0, false
	}
	return cell.Value.Int64, true
}
