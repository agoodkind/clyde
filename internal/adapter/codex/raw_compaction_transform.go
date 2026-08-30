package codex

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"goodkind.io/clyde/internal/reorienttag"
)

// RequiresTerminalValidation reports whether streamed mutation state must be
// validated before accepting the response.
func (t *RawResponsesCompactionTransformer) RequiresTerminalValidation() bool {
	return false
}

// TransformResponse appends the removed transcript to one successful response.
// Upstream failures and response-shape failures retain their original bytes.
func (t *RawResponsesCompactionTransformer) TransformResponse(response *http.Response) *http.Response {
	if t == nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return response
	}
	wrapped := wrappedRawCompactionTranscript(t.transcript)
	encoding, encoded := rawCompactionResponseContentEncoding(response.Header.Get("Content-Encoding"))
	if encoded {
		return t.transformEncodedResponse(response, wrapped, encoding)
	}
	if t.stream || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		clone := *response
		clone.Header = rawCompactionMutatedHeaders(response.Header)
		clone.ContentLength = -1
		clone.Body = newRawCompactionSSEBody(response.Body, wrapped)
		return &clone
	}
	originalBody := response.Body
	body, err := io.ReadAll(originalBody)
	if err != nil {
		response.Body = &rawCompactionReadCloser{reader: io.MultiReader(bytes.NewReader(body), originalBody), closer: originalBody}
		return response
	}
	_ = originalBody.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	transformed, ok := appendRawCompactionJSON(body, wrapped)
	if !ok || bytes.Equal(transformed, body) {
		return response
	}
	clone := *response
	clone.Header = rawCompactionMutatedHeaders(response.Header)
	clone.ContentLength = -1
	clone.Body = io.NopCloser(bytes.NewReader(transformed))
	return &clone
}

func (t *RawResponsesCompactionTransformer) transformEncodedResponse(
	response *http.Response,
	transcriptText string,
	encoding rawCompactionContentEncoding,
) *http.Response {
	streamingResponse := t.stream || strings.Contains(
		strings.ToLower(response.Header.Get("Content-Type")),
		"text/event-stream",
	)
	if streamingResponse {
		decoded, ok := newRawCompactionDecodedStream(response, encoding)
		if !ok {
			return response
		}
		clone := *response
		clone.Header = rawCompactionMutatedHeaders(response.Header)
		clone.ContentLength = -1
		clone.Body = newRawCompactionEncodedBody(
			newRawCompactionSSEBody(decoded, transcriptText),
			encoding,
		)
		return &clone
	}

	originalBody := response.Body
	wireBody, readErr := io.ReadAll(originalBody)
	if readErr != nil {
		response.Body = &rawCompactionReadCloser{reader: io.MultiReader(bytes.NewReader(wireBody), originalBody), closer: originalBody}
		return response
	}
	_ = originalBody.Close()
	decodedBody, ok := decodeRawCompactionBody(wireBody, encoding)
	if !ok {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	transformed, ok := appendRawCompactionJSON(decodedBody, transcriptText)
	if !ok || bytes.Equal(transformed, decodedBody) {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	encodedBody, ok := encodeRawCompactionBody(transformed, encoding)
	if !ok {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	clone := *response
	clone.Header = rawCompactionMutatedHeaders(response.Header)
	clone.ContentLength = -1
	clone.Body = io.NopCloser(bytes.NewReader(encodedBody))
	return &clone
}

func rawCompactionMutatedHeaders(headers http.Header) http.Header {
	clone := headers.Clone()
	for _, name := range []string{"Content-Length", "ETag", "Digest", "Content-Digest"} {
		clone.Del(name)
	}
	return clone
}

func rawCompactionResponseContentEncoding(value string) (rawCompactionContentEncoding, bool) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case string(rawCompactionContentEncodingGzip):
		return rawCompactionContentEncodingGzip, true
	case string(rawCompactionContentEncodingBrotli):
		return rawCompactionContentEncodingBrotli, true
	default:
		return "", false
	}
}

func decodeRawCompactionBody(body []byte, encoding rawCompactionContentEncoding) ([]byte, bool) {
	var reader io.Reader
	switch encoding {
	case rawCompactionContentEncodingGzip:
		gzipReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false
		}
		defer func() { _ = gzipReader.Close() }()
		reader = gzipReader
	case rawCompactionContentEncodingBrotli:
		reader = brotli.NewReader(bytes.NewReader(body))
	default:
		return nil, false
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

func encodeRawCompactionBody(body []byte, encoding rawCompactionContentEncoding) ([]byte, bool) {
	var buffer bytes.Buffer
	writer, ok := newRawCompactionEncodingWriter(&buffer, encoding)
	if !ok {
		return nil, false
	}
	if _, err := writer.Write(body); err != nil {
		_ = writer.Close()
		return nil, false
	}
	if err := writer.Close(); err != nil {
		return nil, false
	}
	return buffer.Bytes(), true
}

func newRawCompactionDecodedStream(
	response *http.Response,
	encoding rawCompactionContentEncoding,
) (io.ReadCloser, bool) {
	switch encoding {
	case rawCompactionContentEncodingGzip:
		buffered := bufio.NewReader(response.Body)
		gzipReader, err := gzip.NewReader(buffered)
		if err != nil {
			response.Body = &rawCompactionReadCloser{reader: buffered, closer: response.Body}
			return nil, false
		}
		return &rawCompactionMultiCloser{
			Reader:  gzipReader,
			closers: []io.Closer{gzipReader, response.Body},
		}, true
	case rawCompactionContentEncodingBrotli:
		return &rawCompactionReadCloser{reader: brotli.NewReader(response.Body), closer: response.Body}, true
	default:
		return nil, false
	}
}

type rawCompactionReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (b *rawCompactionReadCloser) Read(destination []byte) (int, error) {
	count, err := b.reader.Read(destination)
	if errors.Is(err, io.EOF) {
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read raw compaction response: %w", err)
	}
	return count, nil
}

func (b *rawCompactionReadCloser) Close() error {
	if err := b.closer.Close(); err != nil {
		slog.Warn("adapter.codex.raw_compaction.close_failed", "concern", "adapter.providers.codex.request", "err", err)
		return fmt.Errorf("close raw compaction response: %w", err)
	}
	return nil
}

type rawCompactionMultiCloser struct {
	io.Reader
	closers []io.Closer
}

func (b *rawCompactionMultiCloser) Close() error {
	var firstErr error
	for _, closer := range b.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type rawCompactionEncodedBody struct {
	reader *io.PipeReader
	source io.ReadCloser
}

func newRawCompactionEncodedBody(
	source io.ReadCloser,
	encoding rawCompactionContentEncoding,
) io.ReadCloser {
	reader, pipeWriter := io.Pipe()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicErr := fmt.Errorf("encode raw compaction response: %v", recovered)
				slog.Error("adapter.codex.raw_compaction.encode_panic", "concern", "adapter.providers.codex.request", "err", panicErr)
				_ = pipeWriter.CloseWithError(panicErr)
			}
		}()
		defer func() { _ = source.Close() }()
		writer, ok := newRawCompactionEncodingWriter(pipeWriter, encoding)
		if !ok {
			_ = pipeWriter.CloseWithError(errors.New("unsupported raw compaction response encoding"))
			return
		}
		_, copyErr := io.Copy(writer, source)
		closeErr := writer.Close()
		if copyErr != nil {
			_ = pipeWriter.CloseWithError(copyErr)
			return
		}
		_ = pipeWriter.CloseWithError(closeErr)
	}()
	return &rawCompactionEncodedBody{reader: reader, source: source}
}

func (b *rawCompactionEncodedBody) Read(destination []byte) (int, error) {
	count, err := b.reader.Read(destination)
	if errors.Is(err, io.EOF) {
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read encoded raw compaction response: %w", err)
	}
	return count, nil
}

func (b *rawCompactionEncodedBody) Close() error {
	readerErr := b.reader.Close()
	sourceErr := b.source.Close()
	if sourceErr != nil {
		slog.Warn("adapter.codex.raw_compaction.source_close_failed", "concern", "adapter.providers.codex.request", "err", sourceErr)
		return fmt.Errorf("close raw compaction response source: %w", sourceErr)
	}
	if readerErr != nil {
		slog.Warn("adapter.codex.raw_compaction.reader_close_failed", "concern", "adapter.providers.codex.request", "err", readerErr)
		return fmt.Errorf("close encoded raw compaction response: %w", readerErr)
	}
	return nil
}

func newRawCompactionEncodingWriter(
	writer io.Writer,
	encoding rawCompactionContentEncoding,
) (io.WriteCloser, bool) {
	switch encoding {
	case rawCompactionContentEncodingGzip:
		return gzip.NewWriter(writer), true
	case rawCompactionContentEncodingBrotli:
		return brotli.NewWriter(writer), true
	default:
		return nil, false
	}
}

func wrappedRawCompactionTranscript(content string) string {
	return "\n\n" + reorienttag.PreCompactionTranscriptOpen + "\n" + content + "\n" + reorienttag.PreCompactionTranscriptClose + "\n"
}
