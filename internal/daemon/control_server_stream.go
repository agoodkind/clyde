package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/tokencount"
	"goodkind.io/clyde/internal/util"
)

// controlStreamChunkBytes is the payload size of one streamed control-leg chunk.
// It sits well under the gRPC default max message size so a large transcript or
// export crosses the wire as many small frames instead of one oversized message.
const controlStreamChunkBytes = 256 << 10

// StreamConversation resolves one conversation, renders its transcript as plain
// text, and streams that text in bounded chunks so a large transcript is not
// capped by the gRPC max message size. The payload is identical to the unary
// GetConversation text; only the transport differs.
func (s *controlServer) StreamConversation(req *clydev1.GetConversationRequest, stream grpc.ServerStreamingServer[clydev1.ConversationChunk]) error {
	ctx := stream.Context()
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_conversation.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	text, err := s.index.RenderPlainText(record, int(req.GetLastN()))
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_conversation.render_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return status.Errorf(codes.Internal, "render conversation: %v", err)
	}
	return streamTextChunks(stream, text)
}

// StreamConversationContext resolves one conversation, renders the messages
// around a center point as plain text, and streams that text in bounded chunks.
// The payload is identical to the unary GetConversationContext text.
func (s *controlServer) StreamConversationContext(req *clydev1.GetConversationContextRequest, stream grpc.ServerStreamingServer[clydev1.ConversationChunk]) error {
	ctx := stream.Context()
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_context.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	text, err := s.index.ContextWindowText(record, req.GetTimestamp(), int(req.GetMessageIndex()), int(req.GetBefore()), int(req.GetAfter()), req.GetLoadRules())
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_context.render_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return status.Errorf(codes.Internal, "render context: %v", err)
	}
	return streamTextChunks(stream, text)
}

// StreamExportTranscript resolves one conversation, exports its body, and streams
// the body in bounded byte chunks. The payload is identical to the unary
// ExportTranscript body.
func (s *controlServer) StreamExportTranscript(req *clydev1.ExportTranscriptRequest, stream grpc.ServerStreamingServer[clydev1.ExportChunk]) error {
	ctx := stream.Context()
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_export.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	options := conversation.ExportOptions{
		Format:       conversation.ExportFormat(req.GetFormat()),
		HistoryStart: int(req.GetHistoryStart()),
		LastN:        int(req.GetLastN()),
		MaxLines:     int(req.GetMaxLines()),
		MaxTokens:    req.GetMaxTokens(),
		TokenModel:   req.GetTokenModel(),
		Whitespace:   conversation.WhitespaceMode(req.GetWhitespace()),
		Content:      contentKindSetFromExportRequest(req),
		Compaction: conversation.CompactionExportOptions{
			IncludeSelector: req.GetIncludeCompactions(),
			FullHistory:     req.GetFullHistory(),
		},
	}
	body, err := s.index.Export(record, options)
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_export.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return status.Errorf(codes.Internal, "export transcript: %v", err)
	}
	body, err = capExportBodyTokens(ctx, body, req.GetMaxTokens(), req.GetTokenModel(), record, s.exportTokens, s.buildExactCounter)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "max tokens: %v", err)
	}
	return streamExportChunks(stream, body)
}

// Names of the environment variables holding the provider count-API keys, plus
// endpoints. Environment values override configured key files.
const (
	envAnthropic      = "CLYDE_ANTHROPIC_API_KEY"
	envOpenAI         = "CLYDE_OPENAI_API_KEY"
	openAICountURL    = "https://api.openai.com/v1/responses/input_tokens"
	exactCountTimeout = 30 * time.Second
)

// exportTokenConfig holds the resolved configuration for the --max-tokens cap:
// local estimator tuning, the Anthropic count endpoint derived from the adapter
// config, and whether the exact provider APIs may be called.
type exportTokenConfig struct {
	httpClient       *http.Client
	anthropicURL     string
	anthropicVersion string
	openAIURL        string
	exactEnabled     bool
	anthropicAPIKey  string
	openAIAPIKey     string
	settings         tokencount.Settings
}

// newExportTokenConfig derives the export token-cap configuration from the
// daemon config. The Anthropic count endpoint is the sibling of the adapter's
// messages URL.
func newExportTokenConfig(cfg *config.Config) (exportTokenConfig, error) {
	anthropicAPIKey, err := readExportAPIKey(envAnthropic, "export.anthropic_api_key_file", cfg.Export.AnthropicAPIKeyFile)
	if err != nil {
		return exportTokenConfig{}, err
	}
	openAIAPIKey, err := readExportAPIKey(envOpenAI, "export.openai_api_key_file", cfg.Export.OpenAIAPIKeyFile)
	if err != nil {
		return exportTokenConfig{}, err
	}
	anthropicURL := ""
	if messagesURL := cfg.Adapter.Anthropic.OAuth.MessagesURL; messagesURL != "" {
		anthropicURL = messagesURL + "/count_tokens"
	}
	exactEnabled := true
	if cfg.Export.ExactTokenCount != nil {
		exactEnabled = *cfg.Export.ExactTokenCount
	}
	safety := cfg.Export.TokenSafetyFactor
	if safety <= 0 {
		safety = tokencount.DefaultSafetyFactor
	}
	charsPerToken := cfg.Export.HeuristicCharsPerToken
	if charsPerToken <= 0 {
		charsPerToken = tokencount.DefaultCharsPerToken
	}
	return exportTokenConfig{
		httpClient:       &http.Client{Timeout: exactCountTimeout},
		anthropicURL:     anthropicURL,
		anthropicVersion: cfg.Adapter.Anthropic.OAuth.AnthropicVersion,
		openAIURL:        openAICountURL,
		exactEnabled:     exactEnabled,
		anthropicAPIKey:  anthropicAPIKey,
		openAIAPIKey:     openAIAPIKey,
		settings: tokencount.Settings{
			SafetyFactor:  safety,
			CharsPerToken: charsPerToken,
		},
	}, nil
}

func readExportAPIKey(environmentName string, fieldName string, configuredPath string) (string, error) {
	if value := os.Getenv(environmentName); value != "" {
		return value, nil
	}
	if configuredPath == "" {
		return "", nil
	}
	contents, err := os.ReadFile(configuredPath)
	if err != nil {
		return "", fmt.Errorf("read %s: configured file is unavailable", fieldName)
	}
	value := strings.TrimSpace(string(contents))
	if value == "" {
		return "", fmt.Errorf("read %s: configured file is empty", fieldName)
	}
	return value, nil
}

// capExportBodyTokens caps a rendered export body to the max_tokens budget,
// keeping the tail. The tokenizer family and model come from the conversation
// record; a non-empty tokenModel overrides the model. When a provider API key is
// present, the exact count API refines the cap, otherwise the local estimator
// applies. An empty maxTokens or a zero budget leaves the body unchanged. It
// returns an error only when the max_tokens string cannot be parsed.
func capExportBodyTokens(
	ctx context.Context,
	body []byte,
	maxTokens, tokenModel string,
	record conversation.Record,
	cfg exportTokenConfig,
	buildExact func(tokencount.Family) tokencount.ExactCounter,
) ([]byte, error) {
	if maxTokens == "" {
		return body, nil
	}
	budget, err := util.ParseHumanCount(maxTokens)
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_export.max_tokens_invalid", "concern", "process.daemon.lifecycle", "component", "daemon",
			"conversation_id", record.ID,
			"max_tokens", maxTokens,
			"err", err,
		)
		return nil, fmt.Errorf("parse max tokens: %w", err)
	}
	if budget <= 0 {
		return body, nil
	}
	family := tokenFamilyForProvider(record.Provider)
	model := record.Model
	if tokenModel != "" {
		family = tokencount.FamilyUnknown
		model = tokenModel
	}
	effectiveFamily := family
	if effectiveFamily == tokencount.FamilyUnknown {
		effectiveFamily = tokencount.FamilyFromModel(model)
	}
	local := tokencount.LocalCounter(family, model, cfg.settings)
	exact := buildExact(effectiveFamily)

	var capped string
	var truncated bool
	if exact != nil {
		capped, truncated = tokencount.CapToLastTokensExact(ctx, string(body), budget, local, exact, model)
	} else {
		capped, _, truncated = tokencount.CapToLastTokens(string(body), budget, local)
	}
	slog.DebugContext(ctx, "daemon.stream_export.token_cap", "concern", "process.daemon.lifecycle", "component", "daemon",
		"conversation_id", record.ID,
		"budget", budget,
		"family", int(effectiveFamily),
		"model", model,
		"exact", exact != nil,
		"truncated", truncated,
	)
	return []byte(capped), nil
}

// buildExactCounter returns the authoritative count client for a tokenizer
// family, or nil when exact counting is disabled or no API key is set. The
// Anthropic path uses x-api-key auth isolated from the subscription OAuth token.
func (s *controlServer) buildExactCounter(family tokencount.Family) tokencount.ExactCounter {
	if !s.exportTokens.exactEnabled {
		return nil
	}
	switch family {
	case tokencount.FamilyClaude:
		key := s.exportTokens.anthropicAPIKey
		if key == "" || s.exportTokens.anthropicURL == "" {
			return nil
		}
		return tokencount.NewAnthropicExactCounter(s.exportTokens.httpClient, key, s.exportTokens.anthropicURL, s.exportTokens.anthropicVersion)
	case tokencount.FamilyGPT:
		key := s.exportTokens.openAIAPIKey
		if key == "" {
			return nil
		}
		return tokencount.NewOpenAIExactCounter(s.exportTokens.httpClient, key, s.exportTokens.openAIURL)
	case tokencount.FamilyUnknown:
		return nil
	default:
		return nil
	}
}

// tokenFamilyForProvider maps a conversation provider to the tokenizer family
// used to count its transcript. Unmapped providers fall back to model inference.
func tokenFamilyForProvider(provider conversation.Provider) tokencount.Family {
	switch provider {
	case conversation.ProviderClaude:
		return tokencount.FamilyClaude
	case conversation.ProviderCodex:
		return tokencount.FamilyGPT
	default:
		return tokencount.FamilyUnknown
	}
}

// streamTextChunks sends text as a sequence of ConversationChunk frames, each at
// most controlStreamChunkBytes bytes. An empty payload sends no frames, which the
// reassembler reads as an empty string.
func streamTextChunks(stream grpc.ServerStreamingServer[clydev1.ConversationChunk], text string) error {
	body := []byte(text)
	for offset := 0; offset < len(body); offset += controlStreamChunkBytes {
		end := min(offset+controlStreamChunkBytes, len(body))
		if err := stream.Send(&clydev1.ConversationChunk{Text: body[offset:end]}); err != nil {
			slog.WarnContext(stream.Context(), "daemon.stream_text.send_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"err", err,
			)
			return status.Errorf(codes.Internal, "stream conversation chunk: %v", err)
		}
	}
	return nil
}

// streamExportChunks sends body as a sequence of ExportChunk frames, each at most
// controlStreamChunkBytes bytes. An empty body sends no frames, which the
// reassembler reads as an empty byte slice.
func streamExportChunks(stream grpc.ServerStreamingServer[clydev1.ExportChunk], body []byte) error {
	for offset := 0; offset < len(body); offset += controlStreamChunkBytes {
		end := min(offset+controlStreamChunkBytes, len(body))
		if err := stream.Send(&clydev1.ExportChunk{Body: body[offset:end]}); err != nil {
			slog.WarnContext(stream.Context(), "daemon.stream_export.send_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"err", err,
			)
			return status.Errorf(codes.Internal, "stream export chunk: %v", err)
		}
	}
	return nil
}
