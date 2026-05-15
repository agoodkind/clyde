package daemon

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	compactengine "goodkind.io/clyde/internal/compact"
)

// captureCompactStream collects CompactEvent messages emitted by the
// daemon for assertion. It implements the compactEventStream interface
// the server uses to fan events out to the gRPC server-streaming
// channel.
type captureCompactStream struct {
	events []*clydev1.CompactEvent
}

func (c *captureCompactStream) Send(ev *clydev1.CompactEvent) error {
	c.events = append(c.events, ev)
	return nil
}

func TestCompactPreviewRejectsZeroTarget(t *testing.T) {
	srv := newTestServer(t)
	stream := &captureCompactStream{}
	req := &clydev1.CompactRunRequest{
		SessionName:  "zero-target",
		TargetTokens: 0,
		Strippers:    &clydev1.CompactStrippers{},
	}

	err := srv.runCompact(context.Background(), req, stream, compactengine.RuntimeModePreview)
	if err == nil {
		t.Fatalf("runCompact returned nil error for zero target")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("runCompact code = %v, want InvalidArgument", status.Code(err))
	}
	if got := status.Convert(err).Message(); got != "target_tokens must be greater than zero" {
		t.Fatalf("runCompact message = %q, want target validation", got)
	}
	if len(stream.events) != 0 {
		t.Fatalf("runCompact emitted %d events before target validation", len(stream.events))
	}
}
