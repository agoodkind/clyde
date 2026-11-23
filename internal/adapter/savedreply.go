package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

var errReplyChoiceCount = errors.New(
	"save assistant reply metadata: expected one completion choice",
)

func saveReply(
	ctx context.Context,
	logger *slog.Logger,
	provider string,
	content *render.ReplyContent,
	calls []openai.ToolCall,
) (string, error) {
	pending := currentReply(ctx)
	visible := content.Text()

	record := replay.Record{
		Reference:     pending.ref,
		Provider:      provider,
		Digest:        replay.Fingerprint(visible, calls),
		VisibleDigest: replay.TextFingerprint(visible),
		Blocks:        make([]replay.Block, 0, len(content.Blocks)),
	}

	for _, block := range content.Blocks {
		record.Blocks = append(record.Blocks, replay.Block(block))
	}

	if err := pending.store.Save(ctx, record); err != nil {
		logger.WarnContext(ctx, "adapter.replay.save_failed", "provider", provider, "err", err)

		return "", fmt.Errorf("save assistant reply metadata: %w", err)
	}

	return pending.ref.Marker(), nil
}

func (s *Server) saveCollectedReply(
	ctx context.Context,
	provider string,
	response *openai.ChatResponse,
	events []render.Event,
	notices []adapterruntime.UsageNotice,
) error {
	if len(response.Choices) != 1 {
		return errReplyChoiceCount
	}

	message := &response.Choices[0].Message
	var answer string
	if len(message.Content) != 0 {
		if err := json.Unmarshal(message.Content, &answer); err != nil {
			s.log.WarnContext(
				ctx,
				"adapter.replay.decode_reply_failed",
				"provider",
				provider,
				"err",
				err,
			)

			return fmt.Errorf("read collected assistant reply text: %w", err)
		}
	}

	renderer := render.NewEventRendererWithContext(
		ctx,
		response.ID,
		response.Model,
		provider,
		s.log,
	)
	content := renderer.CollectReply(events)
	content.SetCollectedText(answer)

	for _, notice := range notices {
		content.AppendNotice(notice.Text)
		adapterruntime.LogNoticeEmission(ctx, notice, "response_appended")
	}

	marker, err := saveReply(ctx, s.log, provider, content, message.ToolCalls)
	if err != nil {
		return err
	}

	message.Content, err = json.Marshal(content.Text() + marker)
	if err != nil {
		s.log.WarnContext(
			ctx,
			"adapter.replay.encode_reply_failed",
			"provider",
			provider,
			"err",
			err,
		)

		return fmt.Errorf("encode collected assistant reply text: %w", err)
	}

	return nil
}
