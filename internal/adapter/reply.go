package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	"goodkind.io/clyde/internal/adapter/replay"
)

var errForwardedMessageCount = errors.New("forwarded chat message count changed")

// marshalForwardedChatHistory applies the processed assistant content
// to raw chat requests while preserving passthrough extension fields.
func (s *Server) marshalForwardedChatHistory(
	ctx context.Context,
	body []byte,
	req ChatRequest,
) ([]byte, error) {
	var raw passthroughOverrideRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, s.forwardedHistoryError(ctx, "decode request", err)
	}

	var messages []json.RawMessage
	if len(raw["messages"]) == 0 {
		return body, nil
	}

	if err := json.Unmarshal(raw["messages"], &messages); err != nil {
		return nil, s.forwardedHistoryError(ctx, "decode messages", err)
	}

	if len(messages) != len(req.Messages) {
		return nil, errForwardedMessageCount
	}

	for index, message := range req.Messages {
		if message.ReplayParts == nil {
			continue
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(messages[index], &fields); err != nil {
			return nil, s.forwardedHistoryError(ctx, "decode assistant message", err)
		}

		fields["content"] = message.Content
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, s.forwardedHistoryError(ctx, "encode assistant message", err)
		}

		messages[index] = encoded
	}

	var err error
	raw["messages"], err = json.Marshal(messages)
	if err != nil {
		return nil, s.forwardedHistoryError(ctx, "encode messages", err)
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, s.forwardedHistoryError(ctx, "encode request", err)
	}

	return encoded, nil
}

func (s *Server) forwardedHistoryError(ctx context.Context, operation string, err error) error {
	s.log.WarnContext(ctx, "adapter.chat.forwarded_history.failed", "operation", operation)

	return fmt.Errorf("prepare forwarded chat history: %s: %w", operation, err)
}

type replyReplayIngress interface {
	PrepareReplay(
		ctx context.Context,
		log *slog.Logger,
		store replay.Store,
		nativeID string,
		req *ChatRequest,
	) (replay.Reference, error)
}

type pendingReply struct {
	ref   replay.Reference
	store replay.Store
}

type pendingReplyKey struct{}

func currentReply(ctx context.Context) *pendingReply {
	reply, _ := ctx.Value(pendingReplyKey{}).(*pendingReply)

	return reply
}

func (s *Server) prepareReplyReplay(
	ctx context.Context,
	request *http.Request,
	ingress ingresscontract.IngressContract,
	ingressCtx ingresscontract.IngressContext,
	req *ChatRequest,
) (context.Context, error) {
	if ingressLabelFromContext(ctx) != string(ingresscontract.IngressFamilyCursor) {
		return ctx, nil
	}

	history, ok := ingress.(replyReplayIngress)
	if !ok {
		return ctx, adapterErrInternal("reply replay requires an ingress contract", nil)
	}

	nativeID := ingress.TranslateHeaders(request.Header).ConversationID
	if nativeID == "" {
		nativeID = ingressCtx.ConversationID
	}

	store := s.deps.ReplyStore
	if store.Path == "" {
		return ctx, adapterErrInternal("reply replay requires a database path", nil)
	}

	ref, err := history.PrepareReplay(ctx, s.log, store, nativeID, req)
	if err != nil {
		return ctx, adapterErrInternal("could not prepare reply replay", err)
	}

	return context.WithValue(ctx, pendingReplyKey{}, &pendingReply{ref: ref, store: store}), nil
}
