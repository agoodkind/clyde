package capture

import (
	"net/http"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/gklog/correlation"
)

// CappedBuffer accumulates a bounded copy of a streamed body. It implements
// [io.Writer] so it can sit behind an [io.TeeReader], and it never short-writes:
// once the cap is reached it drops the rest and records that it truncated, so
// the wrapping tee never stalls the stream consumer. TotalRead counts every
// byte written, including the bytes past the cap that were dropped.
type CappedBuffer struct {
	buf       []byte
	cap       int
	truncated bool
	totalRead int
}

// NewCappedBuffer returns a buffer that retains at most capBytes.
func NewCappedBuffer(capBytes int) *CappedBuffer {
	return &CappedBuffer{buf: nil, cap: capBytes, truncated: false, totalRead: 0}
}

// Write retains bytes up to the cap and always reports a full write.
func (b *CappedBuffer) Write(p []byte) (int, error) {
	b.totalRead += len(p)
	remaining := b.cap - len(b.buf)
	switch {
	case remaining <= 0:
		b.truncated = true
	case len(p) > remaining:
		b.buf = append(b.buf, p[:remaining]...)
		b.truncated = true
	default:
		b.buf = append(b.buf, p...)
	}
	return len(p), nil
}

// Bytes returns the retained (possibly truncated) copy.
func (b *CappedBuffer) Bytes() []byte { return b.buf }

// Truncated reports whether any bytes were dropped past the cap.
func (b *CappedBuffer) Truncated() bool { return b.truncated }

// TotalRead returns the total number of bytes written, including dropped ones.
func (b *CappedBuffer) TotalRead() int { return b.totalRead }

// Exchange is the provider-neutral view of one captured request/response that
// RecordExchange turns into a Record. Callers fill the provider-specific tags
// (Client, Provider, Concern, UpstreamRequestID, SessionID) and the
// request/response bytes; RecordExchange stamps the correlation ids and timing.
type Exchange struct {
	Client            string
	Provider          string
	Concern           string
	Host              string
	Method            string
	Path              string
	Status            int
	UpstreamRequestID string
	SessionID         string
	RequestHeaders    http.Header
	ResponseHeaders   http.Header
	RequestBody       []byte
	ResponseBody      []byte
	RequestType       string
	ResponseType      string
	Started           time.Time
}

// RecordExchange builds a Record from an Exchange and the request's correlation
// context, stamping Timestamp, RequestID, TraceID, and Duration, then enqueues
// it. A nil store is a no-op. This is the one place adapter egress and ingress
// share for turning an exchange into a stored row.
func (s *Store) RecordExchange(corr correlation.Context, ex Exchange) {
	if s == nil {
		return
	}
	s.Record(Record{
		Timestamp:         clock.Now(),
		Client:            ex.Client,
		Provider:          ex.Provider,
		Concern:           ex.Concern,
		Host:              ex.Host,
		Method:            ex.Method,
		Path:              ex.Path,
		Status:            ex.Status,
		RequestID:         corr.RequestID,
		UpstreamRequestID: ex.UpstreamRequestID,
		SessionID:         ex.SessionID,
		TraceID:           string(corr.TraceID),
		RequestHeaders:    ex.RequestHeaders,
		ResponseHeaders:   ex.ResponseHeaders,
		RequestBody:       ex.RequestBody,
		ResponseBody:      ex.ResponseBody,
		RequestType:       ex.RequestType,
		ResponseType:      ex.ResponseType,
		DecodedRequest:    nil,
		Duration:          clock.Since(ex.Started),
	})
}

// RecordCappedExchange records an exchange whose response body was bounded by
// the caller while it streamed. responseBytes is the full byte count observed.
func (s *Store) RecordCappedExchange(corr correlation.Context, ex Exchange, responseBytes int, responseTruncated bool) {
	if s == nil {
		return
	}
	record := exchangeRecord(corr, ex)
	s.recordWithMetadata(record, len(ex.RequestBody), responseBytes, false, responseTruncated)
}

func exchangeRecord(corr correlation.Context, ex Exchange) Record {
	return Record{
		Timestamp: clock.Now(), Client: ex.Client, Provider: ex.Provider, Concern: ex.Concern,
		Host: ex.Host, Method: ex.Method, Path: ex.Path, Status: ex.Status, RequestID: corr.RequestID,
		UpstreamRequestID: ex.UpstreamRequestID, SessionID: ex.SessionID, TraceID: string(corr.TraceID),
		RequestHeaders: ex.RequestHeaders, ResponseHeaders: ex.ResponseHeaders, RequestBody: ex.RequestBody,
		ResponseBody: ex.ResponseBody, RequestType: ex.RequestType, ResponseType: ex.ResponseType,
		Duration: clock.Since(ex.Started),
	}
}
