package mitm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
)

// forwardAndCaptureProviderResponse streams the upstream response back to the
// intercepted client and, in parallel, buffers the raw response body (up to
// bodyCap) so the completed exchange can be persisted to the SQLite capture
// store. It returns the total wire bytes written to the client and the
// buffered body bytes. Capture is best-effort: the body buffer caps silently
// and never affects what the client receives.
func (p *Proxy) forwardAndCaptureProviderResponse(client *bufio.Writer, resp *http.Response, bodyCap int) (int64, []byte, error) {
	bodyBuffer := &limitedBuffer{limit: bodyCap, buf: bytes.Buffer{}}
	chunked := resp.ContentLength < 0
	header := providerResponseHeader(resp, chunked)
	responseBytes, err := writeProviderResponseBytes(client, header, "header")
	if err != nil {
		return 0, bodyBuffer.Bytes(), err
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, _ = bodyBuffer.Write(buf[:n])
			written, err := writeProviderResponseBodyChunk(client, buf[:n], chunked)
			responseBytes += written
			if err != nil {
				return responseBytes, bodyBuffer.Bytes(), err
			}
		}
		if errors.Is(readErr, io.EOF) {
			written, err := writeProviderResponseEOF(client, chunked)
			responseBytes += written
			if err != nil {
				return responseBytes, bodyBuffer.Bytes(), err
			}
			return responseBytes, bodyBuffer.Bytes(), nil
		}
		if readErr != nil {
			p.log.Warn("mitm.provider.response.read_body_failed", "concern", "providers.mitm.wire", "err", readErr)
			return responseBytes, bodyBuffer.Bytes(), fmt.Errorf("read provider response body: %w", readErr)
		}
	}
}

func providerResponseHeader(resp *http.Response, chunked bool) []byte {
	headers := resp.Header.Clone()
	headers.Del("Transfer-Encoding")
	if chunked {
		headers.Del("Content-Length")
		header := headerBlock(resp.Proto+" "+resp.Status+"\r\n", headers)
		return append(header[:len(header)-2], []byte("Transfer-Encoding: chunked\r\n\r\n")...)
	}
	headers.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	return headerBlock(resp.Proto+" "+resp.Status+"\r\n", headers)
}

func writeProviderResponseBodyChunk(client *bufio.Writer, chunk []byte, chunked bool) (int64, error) {
	var written int64
	if chunked {
		chunkHeader := fmt.Appendf(nil, "%x\r\n", len(chunk))
		count, err := writeProviderResponseBytes(client, chunkHeader, "chunk header")
		written += count
		if err != nil {
			return written, err
		}
	}
	count, err := writeProviderResponseBytes(client, chunk, "body")
	written += count
	if err != nil {
		return written, err
	}
	if chunked {
		count, err = writeProviderResponseString(client, "\r\n", "chunk terminator")
		written += count
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func writeProviderResponseEOF(client *bufio.Writer, chunked bool) (int64, error) {
	if !chunked {
		if err := client.Flush(); err != nil {
			slog.Warn("mitm.provider.response.flush_body_failed", "concern", "providers.mitm.wire", "err", err)
			return 0, fmt.Errorf("flush provider response body: %w", err)
		}
		return 0, nil
	}
	return writeProviderResponseString(client, "0\r\n\r\n", "chunk EOF")
}

func writeProviderResponseBytes(client *bufio.Writer, chunk []byte, label string) (int64, error) {
	if _, err := client.Write(chunk); err != nil {
		slog.Warn("mitm.provider.response.write_client_failed", "concern", "providers.mitm.wire", "label", label, "err", err)
		return 0, fmt.Errorf("write provider response %s: %w", label, err)
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.provider.response.flush_failed", "concern", "providers.mitm.wire", "label", label, "err", err)
		return 0, fmt.Errorf("flush provider response %s: %w", label, err)
	}
	return int64(len(chunk)), nil
}

func writeProviderResponseString(client *bufio.Writer, text string, label string) (int64, error) {
	if _, err := client.WriteString(text); err != nil {
		slog.Warn("mitm.provider.response.write_client_failed", "concern", "providers.mitm.wire", "label", label, "err", err)
		return 0, fmt.Errorf("write provider response %s: %w", label, err)
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.provider.response.flush_failed", "concern", "providers.mitm.wire", "label", label, "err", err)
		return 0, fmt.Errorf("flush provider response %s: %w", label, err)
	}
	return int64(len(text)), nil
}
