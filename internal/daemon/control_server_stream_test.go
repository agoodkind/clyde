package daemon

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
)

// recordingConversationStream is a server-stream fake that records each sent
// ConversationChunk so a test can assert the chunk boundaries and reassemble the
// payload. It embeds the generated interface so the unused stream methods satisfy
// the type without being implemented; only Send and Context are exercised.
type recordingConversationStream struct {
	grpc.ServerStreamingServer[clydev1.ConversationChunk]
	chunks [][]byte
}

func (r *recordingConversationStream) Send(chunk *clydev1.ConversationChunk) error {
	r.chunks = append(r.chunks, append([]byte(nil), chunk.GetText()...))
	return nil
}

func (r *recordingConversationStream) Context() context.Context { return context.Background() }

// recordingExportStream mirrors recordingConversationStream for ExportChunk.
type recordingExportStream struct {
	grpc.ServerStreamingServer[clydev1.ExportChunk]
	chunks [][]byte
}

func (r *recordingExportStream) Send(chunk *clydev1.ExportChunk) error {
	r.chunks = append(r.chunks, append([]byte(nil), chunk.GetBody()...))
	return nil
}

func (r *recordingExportStream) Context() context.Context { return context.Background() }

// replayConversationClient is a client-stream fake that replays a fixed list of
// ConversationChunk frames and then io.EOF, so a test can drive the reassembly
// helper without a live daemon.
type replayConversationClient struct {
	grpc.ServerStreamingClient[clydev1.ConversationChunk]
	chunks [][]byte
	cursor int
}

func (r *replayConversationClient) Recv() (*clydev1.ConversationChunk, error) {
	if r.cursor >= len(r.chunks) {
		return nil, io.EOF
	}
	chunk := &clydev1.ConversationChunk{Text: r.chunks[r.cursor]}
	r.cursor++
	return chunk, nil
}

func (r *replayConversationClient) Context() context.Context { return context.Background() }

// replayExportClient mirrors replayConversationClient for ExportChunk.
type replayExportClient struct {
	grpc.ServerStreamingClient[clydev1.ExportChunk]
	chunks [][]byte
	cursor int
}

func (r *replayExportClient) Recv() (*clydev1.ExportChunk, error) {
	if r.cursor >= len(r.chunks) {
		return nil, io.EOF
	}
	chunk := &clydev1.ExportChunk{Body: r.chunks[r.cursor]}
	r.cursor++
	return chunk, nil
}

func (r *replayExportClient) Context() context.Context { return context.Background() }

type recordingProviderStatsStream struct {
	grpc.ServerStreamingServer[clydev1.ProviderStatsEvent]
	ctx    context.Context
	cancel context.CancelFunc
	events []*clydev1.ProviderStatsEvent
}

func (r *recordingProviderStatsStream) Send(event *clydev1.ProviderStatsEvent) error {
	r.events = append(r.events, event)
	if len(r.events) == 2 {
		r.cancel()
	}
	return nil
}

func (r *recordingProviderStatsStream) Context() context.Context { return r.ctx }

func TestSubscribeProviderStatsUsesEmissionTime(t *testing.T) {
	generationStartedAt := time.Unix(100, 0)
	recorder := &providerStatsRecorder{
		stats:               map[string]ProviderStats{"codex": {ProviderDetail: "codex"}},
		generationStartedAt: generationStartedAt,
		now:                 func() time.Time { return generationStartedAt },
	}
	emissionTimes := []time.Time{time.Unix(200, 0), time.Unix(300, 0)}
	emissionIndex := 0
	ticks := make(chan time.Time, 1)
	ticks <- emissionTimes[1]
	ctx, cancel := context.WithCancel(context.Background())
	stream := &recordingProviderStatsStream{ctx: ctx, cancel: cancel}
	server := &controlServer{
		stats: recorder,
		providerStatsNow: func() time.Time {
			result := emissionTimes[emissionIndex]
			emissionIndex++
			return result
		},
		providerStatsTicks: ticks,
	}

	err := server.SubscribeProviderStats(&clydev1.SubscribeProviderStatsRequest{}, stream)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("SubscribeProviderStats() error = %v, want context canceled", err)
	}
	if len(stream.events) != 2 {
		t.Fatalf("event count = %d, want 2", len(stream.events))
	}
	for i, event := range stream.events {
		if event.GetEmittedAtUnix() != emissionTimes[i].Unix() {
			t.Fatalf("event %d emitted_at_unix = %d, want %d", i, event.GetEmittedAtUnix(), emissionTimes[i].Unix())
		}
		if event.GetEmittedAtUnix() == generationStartedAt.Unix() {
			t.Fatalf("event %d reused generation start", i)
		}
	}
}

func TestStreamTextChunksRoundTrips(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":           "",
		"single_byte":     "x",
		"under_one_chunk": strings.Repeat("a", controlStreamChunkBytes-1),
		"exact_one_chunk": strings.Repeat("b", controlStreamChunkBytes),
		"just_over":       strings.Repeat("c", controlStreamChunkBytes+1),
		"many_chunks":     strings.Repeat("d", controlStreamChunkBytes*3+7),
		// A multibyte rune straddling a chunk boundary must survive: the byte
		// stream is split at controlStreamChunkBytes, which can fall inside a
		// 3-byte rune, and the reassembler stitches the bytes before decoding.
		"multibyte_boundary": strings.Repeat("a", controlStreamChunkBytes-1) + strings.Repeat("世", 4),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := &recordingConversationStream{ServerStreamingServer: nil, chunks: nil}
			if err := streamTextChunks(server, text); err != nil {
				t.Fatalf("streamTextChunks: %v", err)
			}
			assertChunkBounds(t, server.chunks)
			client := &replayConversationClient{ServerStreamingClient: nil, chunks: server.chunks, cursor: 0}
			got, err := reassembleConversationChunks(context.Background(), client)
			if err != nil {
				t.Fatalf("reassembleConversationChunks: %v", err)
			}
			if got != text {
				t.Fatalf("reassembled text length %d, want %d", len(got), len(text))
			}
		})
	}
}

func TestStreamExportChunksRoundTrips(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"empty":           {},
		"single_byte":     {0x01},
		"under_one_chunk": repeatByte(0x02, controlStreamChunkBytes-1),
		"exact_one_chunk": repeatByte(0x03, controlStreamChunkBytes),
		"just_over":       repeatByte(0x04, controlStreamChunkBytes+1),
		"many_chunks":     repeatByte(0x05, controlStreamChunkBytes*2+13),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := &recordingExportStream{ServerStreamingServer: nil, chunks: nil}
			if err := streamExportChunks(server, body); err != nil {
				t.Fatalf("streamExportChunks: %v", err)
			}
			assertChunkBounds(t, server.chunks)
			client := &replayExportClient{ServerStreamingClient: nil, chunks: server.chunks, cursor: 0}
			got, err := reassembleExportChunks(context.Background(), client)
			if err != nil {
				t.Fatalf("reassembleExportChunks: %v", err)
			}
			if len(got) != len(body) {
				t.Fatalf("reassembled body length %d, want %d", len(got), len(body))
			}
			for i := range got {
				if got[i] != body[i] {
					t.Fatalf("reassembled body differs at index %d", i)
				}
			}
		})
	}
}

// assertChunkBounds verifies that no chunk exceeds the size bound and that no
// empty chunk is sent, which would waste a frame.
func assertChunkBounds(t *testing.T, chunks [][]byte) {
	t.Helper()
	for index, chunk := range chunks {
		if len(chunk) == 0 {
			t.Fatalf("chunk %d is empty", index)
		}
		if len(chunk) > controlStreamChunkBytes {
			t.Fatalf("chunk %d is %d bytes, over the %d bound", index, len(chunk), controlStreamChunkBytes)
		}
	}
}

func repeatByte(value byte, count int) []byte {
	body := make([]byte, count)
	for i := range body {
		body[i] = value
	}
	return body
}
