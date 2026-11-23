package parser

import (
	"context"
	"log/slog"
	"strings"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
)

// projectAssistantText removes Clyde's replay reference and separates
// verified thinking from assistant prose.
func projectAssistantText(
	ctx context.Context,
	text string,
	nativeThinking string,
) (string, string) {
	visible, reference, hasReference := replay.StripReference(text)
	if !hasReference {
		return stripClydeSyntheticEnvelopes(text), nativeThinking
	}

	record, found, err := replay.DefaultStore().Load(ctx, reference)
	if err != nil {
		slog.WarnContext(
			ctx,
			"providers.cursor.parser.load_replay_reply_failed",
			"concern",
			concern,
			"err",
			err,
		)

		return visible, nativeThinking
	}

	if !found {
		return visible, nativeThinking
	}

	parts, valid := record.Parts(visible)
	if !valid {
		return visible, nativeThinking
	}

	var prose strings.Builder
	var thinking []string

	for _, part := range parts {
		switch part.Kind {
		case adapterrender.SyntheticKindText:
			prose.WriteString(part.Body)
		case adapterrender.SyntheticKindThinking:
			if nativeThinking == "" && part.Body != "" {
				thinking = append(thinking, part.Body)
			}
		}
	}

	if nativeThinking == "" {
		nativeThinking = strings.Join(thinking, "\n\n")
	}

	return prose.String(), nativeThinking
}

// stripClydeSyntheticEnvelopes removes complete legacy envelopes. It inserts a
// blank line between the surrounding text spans because the envelope parser
// removes the separator after each envelope.
func stripClydeSyntheticEnvelopes(text string) string {
	parts := adapterrender.ExtractSyntheticParts(text)
	textBodies := make([]string, 0, len(parts))

	for _, part := range parts {
		if part.Kind == adapterrender.SyntheticKindText {
			textBodies = append(textBodies, part.Body)
		}
	}

	return strings.Join(textBodies, "\n\n")
}
