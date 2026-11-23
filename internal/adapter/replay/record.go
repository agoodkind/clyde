package replay

import "goodkind.io/clyde/internal/adapter/render"

// Record stores provider metadata for one completed assistant reply.
type Record struct {
	Reference     Reference
	Provider      string
	Digest        string
	VisibleDigest string
	Blocks        []Block
}

// Block records the byte range that Clyde generated for one provider event.
// An event without a visible representation has equal Start and End offsets.
type Block struct {
	Start int                  `json:"start"`
	End   int                  `json:"end"`
	Part  render.SyntheticPart `json:"part"`
}

// Parts reconstructs provider blocks and the surrounding assistant text.
//
// It verifies the visible text and each byte range before returning any parts.
//
//	parts, matched := record.Parts(visible)
func (record Record) Parts(visible string) ([]render.SyntheticPart, bool) {
	if TextFingerprint(visible) != record.VisibleDigest {
		return nil, false
	}

	parts := make([]render.SyntheticPart, 0, 2*len(record.Blocks)+1)
	offset := 0

	for _, block := range record.Blocks {
		if block.Start < offset || block.End < block.Start || block.End > len(visible) {
			return nil, false
		}

		if block.Start > offset {
			parts = append(parts, textPart(visible[offset:block.Start]))
		}

		parts = append(parts, block.Part)
		offset = block.End
	}

	if offset < len(visible) {
		parts = append(parts, textPart(visible[offset:]))
	}

	return parts, true
}

func textPart(body string) render.SyntheticPart {
	var part render.SyntheticPart
	part.Kind = render.SyntheticKindText
	part.Body = body

	return part
}
