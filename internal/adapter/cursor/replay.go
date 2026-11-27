package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	adaptercontent "goodkind.io/clyde/internal/adapter/content"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
)

var errInvalidLegacyReply = errors.New("verify imported legacy Cursor reply")

// PrepareReplay resolves the metadata for historical Cursor replies.
//
// It removes replay references from assistant messages and returns a reference
// for the new reply. A native conversation ID takes precedence when available.
//
//	ref, err := ingress.PrepareReplay(ctx, log, store, nativeID, &request)
func (i *Ingress) PrepareReplay(
	ctx context.Context,
	log *slog.Logger,
	store replay.Store,
	nativeID string,
	req *adapteropenai.ChatRequest,
) (replay.Reference, error) {
	conversationID := nativeID

	for index := range req.Messages {
		message := &req.Messages[index]
		if message.Role != "assistant" {
			continue
		}

		ref, err := prepareHistoricalReply(ctx, log, store, nativeID, message)
		if err != nil {
			var empty replay.Reference

			return empty, err
		}

		if nativeID == "" && ref.ConversationID != "" {
			conversationID = ref.ConversationID
		}
	}

	if conversationID == "" {
		conversationID = replay.NewID()
	}

	return replay.Reference{ConversationID: conversationID, ReplyID: replay.NewID()}, nil
}

func prepareHistoricalReply(
	ctx context.Context,
	log *slog.Logger,
	store replay.Store,
	nativeID string,
	message *adapteropenai.ChatMessage,
) (replay.Reference, error) {
	var empty replay.Reference
	original, textOnly := assistantText(message.Content)
	if !textOnly {
		return empty, nil
	}

	visible, ref, referenced := replay.StripReference(original)
	if !referenced {
		if strings.TrimSpace(original) == "" {
			return recoverToolReply(ctx, log, store, nativeID, message), nil
		}

		return empty, importLegacyReply(ctx, log, store, message, original)
	}

	// Preserve ordinary Markdown, including any legacy syntax in a new reply.
	if err := marshalReplyText(ctx, log, message, visible); err != nil {
		return empty, err
	}

	if nativeID != "" && nativeID != ref.ConversationID {
		logReplayMiss(ctx, log, "conversation_mismatch")

		return empty, nil
	}

	parts, reason := loadReplyParts(ctx, store, ref, visible, message.ToolCalls)
	if reason != "" {
		logReplayMiss(ctx, log, reason)

		return ref, nil
	}

	message.ReplayParts = parts

	return ref, nil
}

func recoverToolReply(
	ctx context.Context,
	log *slog.Logger,
	store replay.Store,
	nativeID string,
	message *adapteropenai.ChatMessage,
) replay.Reference {
	var empty replay.Reference
	digest := replay.ToolFingerprint(message.ToolCalls)
	if digest == "" {
		return empty
	}

	record, found, err := store.FindToolReply(ctx, nativeID, digest)
	if err != nil {
		logReplayMiss(ctx, log, "tool_read_failed")

		return empty
	}

	if !found {
		return empty
	}

	parts := make([]render.SyntheticPart, 0, len(record.Blocks))
	for _, block := range record.Blocks {
		if block.Part.Kind == render.SyntheticKindThinking ||
			block.Part.Kind == render.SyntheticRedactedThinking ||
			block.Part.Kind == render.SyntheticToolSignature {
			parts = append(parts, block.Part)
		}
	}

	message.ReplayParts = parts

	return record.Reference
}

func loadReplyParts(
	ctx context.Context,
	store replay.Store,
	ref replay.Reference,
	visible string,
	calls []adapteropenai.ToolCall,
) ([]render.SyntheticPart, string) {
	record, found, err := store.Load(ctx, ref)
	if err != nil {
		return nil, "read_failed"
	}

	if !found {
		return nil, "missing_reply"
	}

	if record.Digest != replay.Fingerprint(visible, calls) {
		return nil, "reply_changed"
	}

	parts, valid := record.Parts(visible)
	if !valid {
		return nil, "invalid_blocks"
	}

	return parts, ""
}

func logReplayMiss(ctx context.Context, log *slog.Logger, reason string) {
	log.WarnContext(ctx, "adapter.cursor.replay.lookup_failed", "reason", reason)
}

func assistantText(raw json.RawMessage) (string, bool) {
	parts, kind := adaptercontent.NormalizeRaw(raw)
	if kind == adaptercontent.KindEmpty {
		return "", true
	}

	if kind != adaptercontent.KindString && kind != adaptercontent.KindParts {
		return "", false
	}

	var combined strings.Builder

	for _, part := range parts {
		if part.Kind != adaptercontent.PartText {
			return "", false
		}

		combined.WriteString(part.Text)
	}

	return combined.String(), true
}

func marshalReplyText(
	ctx context.Context,
	log *slog.Logger,
	message *adapteropenai.ChatMessage,
	visible string,
) error {
	raw, err := json.Marshal(visible)
	if err != nil {
		return replayError(ctx, log, "encode visible reply", err)
	}

	message.Content = raw
	var part render.SyntheticPart
	part.Kind = render.SyntheticKindText
	part.Body = visible
	message.ReplayParts = []render.SyntheticPart{part}

	return nil
}

func importLegacyReply(
	ctx context.Context,
	log *slog.Logger,
	store replay.Store,
	message *adapteropenai.ChatMessage,
	original string,
) error {
	spans := render.LegacySyntheticSpans(original)
	if len(spans) == 0 {
		return nil
	}

	digest := replay.Fingerprint(original, message.ToolCalls)
	record := replay.Record{
		Reference:     replay.Reference{ConversationID: "clyde-legacy", ReplyID: digest},
		Provider:      "",
		Digest:        digest,
		VisibleDigest: replay.TextFingerprint(original),
		ToolDigest:    "",
		Blocks:        make([]replay.Block, 0, len(spans)),
	}

	for _, span := range spans {
		record.Blocks = append(
			record.Blocks,
			replay.Block{Start: span.Start, End: span.End, Part: span.Part},
		)
	}

	saved, found, err := store.Load(ctx, record.Reference)
	if err != nil {
		return replayError(ctx, log, "look up legacy reply", err)
	}

	if found {
		record = saved
	} else if err := store.Save(ctx, record); err != nil {
		return replayError(ctx, log, "import legacy reply", err)
	}

	parts, valid := record.Parts(original)
	if !valid {
		return errInvalidLegacyReply
	}

	var text strings.Builder

	for _, part := range parts {
		if part.Kind == render.SyntheticKindText || part.Kind == render.SyntheticKindThinking {
			text.WriteString(part.Body)
		}
	}

	if err := marshalReplyText(ctx, log, message, text.String()); err != nil {
		return err
	}

	message.ReplayParts = parts

	return nil
}

func replayError(ctx context.Context, log *slog.Logger, operation string, err error) error {
	log.WarnContext(ctx, "adapter.cursor.replay.failed", "operation", operation)

	return fmt.Errorf("cursor replay: %s: %w", operation, err)
}
