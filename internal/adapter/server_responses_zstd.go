package adapter

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

const (
	maxResponsesRequestBodyBytes  = 8 * 1024 * 1024
	maxResponsesResponseBodyBytes = 8 * 1024 * 1024
)

func readResponsesRequestBody(body []byte, contentEncoding string) ([]byte, error) {
	if !nativeResponsesZstdEncoded(contentEncoding) {
		return body, nil
	}
	decoder, err := zstd.NewReader(
		bytes.NewReader(body),
		zstd.WithDecoderMaxMemory(maxResponsesRequestBodyBytes),
	)
	if err != nil {
		slog.Warn("adapter.openai.responses_zstd_decode_failed", "concern", "adapter.chat.dispatch", "err", err)
		return nil, fmt.Errorf("create zstd request decoder: %w", err)
	}
	defer decoder.Close()
	decoded, err := io.ReadAll(io.LimitReader(decoder, maxResponsesRequestBodyBytes+1))
	if err != nil {
		slog.Warn("adapter.openai.responses_zstd_decode_failed", "concern", "adapter.chat.dispatch", "err", err)
		return nil, fmt.Errorf("decode zstd request body: %w", err)
	}
	if len(decoded) > maxResponsesRequestBodyBytes {
		return nil, errors.New("decoded zstd request body exceeds limit")
	}
	return decoded, nil
}

func prepareNativeCodexResponsesCompaction(
	raw adaptercodex.RawResponsesRequest,
	decodedBody []byte,
	settings adaptercodex.RawResponsesCompactionSettings,
) (adaptercodex.RawResponsesRequest, *adaptercodex.RawResponsesCompactionTransformer, *adaptercodex.RawResponsesCompactionV2Plan) {
	decodedRaw := raw
	decodedRaw.Body = decodedBody
	if adaptercodex.DetectRawResponsesCompactionProtocol(decodedRaw.Header) == adaptercodex.RawResponsesCompactionV2 {
		return prepareNativeCodexResponsesCompactionV2(raw, decodedRaw, decodedBody, settings)
	}
	transformed, transformer := adaptercodex.PrepareRawResponsesCompaction(decodedRaw, settings)
	if transformer == nil {
		return raw, nil, nil
	}
	forwardBody, ok := encodeNativeResponsesBody(transformed.Body, raw.Header.Get("Content-Encoding"))
	if !ok {
		return raw, nil, nil
	}
	transformed.Body = forwardBody
	return transformed, transformer, nil
}

func prepareNativeCodexResponsesCompactionV2(
	raw adaptercodex.RawResponsesRequest,
	decodedRaw adaptercodex.RawResponsesRequest,
	decodedBody []byte,
	settings adaptercodex.RawResponsesCompactionSettings,
) (adaptercodex.RawResponsesRequest, *adaptercodex.RawResponsesCompactionV2Plan) {
	plan, ok := adaptercodex.PlanRawResponsesCompactionV2(decodedRaw, settings)
	if !ok {
		return raw, nil
	}
	if bytes.Equal(plan.Request.Body, decodedBody) {
		plan.Request.Body = raw.Body
		return plan.Request, &plan
	}
	forwardBody, ok := encodeNativeResponsesBody(plan.Request.Body, raw.Header.Get("Content-Encoding"))
	if !ok {
		return raw, nil
	}
	plan.Request.Body = forwardBody
	return plan.Request, &plan
}

func encodeNativeResponsesBody(body []byte, contentEncoding string) ([]byte, bool) {
	if !nativeResponsesZstdEncoded(contentEncoding) {
		return body, true
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, false
	}
	defer encoder.Close()
	return encoder.EncodeAll(body, nil), true
}

func transformNativeCodexCompactionResponse(
	response *http.Response,
	transformer *adaptercodex.RawResponsesCompactionTransformer,
	requestStreams bool,
) *http.Response {
	if response == nil || !nativeResponsesZstdEncoded(response.Header.Get("Content-Encoding")) {
		return transformer.TransformResponse(response)
	}
	if requestStreams || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return transformStreamingNativeCodexCompactionResponse(response, transformer)
	}
	originalBody := response.Body
	wireBody, err := io.ReadAll(io.LimitReader(originalBody, maxResponsesResponseBodyBytes+1))
	if err != nil {
		response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(wireBody), originalBody))
		return response
	}
	if len(wireBody) > maxResponsesResponseBodyBytes {
		response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(wireBody), originalBody))
		return response
	}
	_ = originalBody.Close()
	response.Body = io.NopCloser(bytes.NewReader(wireBody))
	decoder, err := zstd.NewReader(
		bytes.NewReader(wireBody),
		zstd.WithDecoderMaxMemory(maxResponsesResponseBodyBytes),
	)
	if err != nil {
		return response
	}
	decodedBody, err := io.ReadAll(io.LimitReader(decoder, maxResponsesResponseBodyBytes+1))
	decoder.Close()
	if err != nil || len(decodedBody) > maxResponsesResponseBodyBytes {
		return response
	}
	decodedResponse := *response
	decodedResponse.Header = response.Header.Clone()
	decodedResponse.Header.Del("Content-Encoding")
	decodedResponse.Header.Del("Content-Length")
	decodedResponse.ContentLength = -1
	decodedResponse.Body = io.NopCloser(bytes.NewReader(decodedBody))
	transformed := transformer.TransformResponse(&decodedResponse)
	transformedBody, err := io.ReadAll(transformed.Body)
	_ = transformed.Body.Close()
	if err != nil || bytes.Equal(transformedBody, decodedBody) {
		return response
	}
	transformed.Body = io.NopCloser(bytes.NewReader(transformedBody))
	return transformed
}

func transformStreamingNativeCodexCompactionResponse(
	response *http.Response,
	transformer *adaptercodex.RawResponsesCompactionTransformer,
) *http.Response {
	if !transformer.RequiresTerminalValidation() {
		var consumed bytes.Buffer
		buffered := bufio.NewReader(io.TeeReader(response.Body, &consumed))
		decoder, err := zstd.NewReader(buffered, zstd.WithDecoderMaxMemory(maxResponsesResponseBodyBytes))
		if err != nil {
			response.Body = &nativeResponsesPassthroughBody{Reader: io.MultiReader(bytes.NewReader(consumed.Bytes()), response.Body), source: response.Body}
			return response
		}
		probe := make([]byte, 1)
		count, readErr := decoder.Read(probe)
		if readErr != nil && readErr != io.EOF {
			decoder.Close()
			response.Body = &nativeResponsesPassthroughBody{Reader: io.MultiReader(bytes.NewReader(consumed.Bytes()), response.Body), source: response.Body}
			return response
		}
		decodedResponse := *response
		decodedResponse.Header = response.Header.Clone()
		decodedResponse.Header.Del("Content-Encoding")
		decodedResponse.Header.Del("Content-Length")
		decodedResponse.ContentLength = -1
		decodedResponse.Body = &nativeResponsesZstdBody{ReadCloser: io.NopCloser(io.MultiReader(bytes.NewReader(probe[:count]), decoder.IOReadCloser())), source: response.Body, decoder: decoder}
		return transformer.TransformResponse(&decodedResponse)
	}
	originalBody := response.Body
	wireBody, err := io.ReadAll(io.LimitReader(originalBody, maxResponsesResponseBodyBytes+1))
	_ = originalBody.Close()
	if err != nil || len(wireBody) > maxResponsesResponseBodyBytes {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	decoder, err := zstd.NewReader(
		bytes.NewReader(wireBody),
		zstd.WithDecoderMaxMemory(maxResponsesResponseBodyBytes),
	)
	if err != nil {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	decodedBody, err := io.ReadAll(io.LimitReader(decoder, maxResponsesResponseBodyBytes+1))
	decoder.Close()
	if err != nil || len(decodedBody) > maxResponsesResponseBodyBytes {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	decodedResponse := *response
	decodedResponse.Header = response.Header.Clone()
	decodedResponse.Header.Del("Content-Encoding")
	decodedResponse.Header.Del("Content-Length")
	decodedResponse.ContentLength = -1
	decodedResponse.Body = io.NopCloser(bytes.NewReader(decodedBody))
	transformed := transformer.TransformResponse(&decodedResponse)
	transformedBody, err := io.ReadAll(transformed.Body)
	_ = transformed.Body.Close()
	if err != nil || bytes.Equal(transformedBody, decodedBody) {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	encodedBody := encoder.EncodeAll(transformedBody, nil)
	encoder.Close()
	clone := *response
	clone.Header = response.Header.Clone()
	clone.Header.Del("Content-Length")
	clone.ContentLength = -1
	clone.Body = io.NopCloser(bytes.NewReader(encodedBody))
	return &clone
}

type nativeResponsesZstdBody struct {
	io.ReadCloser
	source  io.Closer
	decoder *zstd.Decoder
}

func (b *nativeResponsesZstdBody) Close() error {
	decoderErr := b.ReadCloser.Close()
	if b.decoder != nil {
		b.decoder.Close()
	}
	sourceErr := b.source.Close()
	if decoderErr != nil {
		slog.Warn("adapter.openai.responses_zstd_close_failed", "concern", "adapter.chat.dispatch", "err", decoderErr)
		return fmt.Errorf("close zstd decoder: %w", decoderErr)
	}
	if sourceErr != nil {
		slog.Warn("adapter.openai.responses_zstd_close_failed", "concern", "adapter.chat.dispatch", "err", sourceErr)
		return fmt.Errorf("close encoded response body: %w", sourceErr)
	}
	return nil
}

type nativeResponsesPassthroughBody struct {
	io.Reader
	source io.Closer
}

func (b *nativeResponsesPassthroughBody) Close() error {
	return b.source.Close()
}

func nativeResponsesZstdEncoded(contentEncoding string) bool {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	return encoding == "zstd" || encoding == "zstandard"
}
