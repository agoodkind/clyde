package ui

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPolishDaemonErrorNil(t *testing.T) {
	got := polishDaemonError(nil)
	if got != "" {
		t.Fatalf("expected empty string for nil error, got %q", got)
	}
}

func TestPolishDaemonErrorFailedPreconditionSessionOpen(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, `session "configs-dd8c61fc" is currently open; exit it first or pass --force`)
	got := polishDaemonError(err)
	want := "session in use; close it before continuing"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestPolishDaemonErrorFailedPreconditionSessionInUse(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, "session in use by another holder")
	got := polishDaemonError(err)
	want := "session in use; close it before continuing"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestPolishDaemonErrorFailedPreconditionOther(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, "preview not staged")
	got := polishDaemonError(err)
	want := "preview not staged"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestPolishDaemonErrorNotFound(t *testing.T) {
	err := status.Error(codes.NotFound, `session "missing" not found`)
	got := polishDaemonError(err)
	want := "session not found"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestPolishDaemonErrorUnavailable(t *testing.T) {
	err := status.Error(codes.Unavailable, "transport closing")
	got := polishDaemonError(err)
	want := "daemon unavailable; check that clyde-daemon is running"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestPolishDaemonErrorDeadlineExceeded(t *testing.T) {
	err := status.Error(codes.DeadlineExceeded, "context deadline exceeded")
	got := polishDaemonError(err)
	want := "request timed out; try again"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestPolishDaemonErrorUnknownNonStatusFallsThrough(t *testing.T) {
	err := errors.New("plain old error")
	got := polishDaemonError(err)
	want := "plain old error"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestPolishDaemonErrorOtherCodeReturnsMessage(t *testing.T) {
	err := status.Error(codes.PermissionDenied, "not allowed")
	got := polishDaemonError(err)
	want := "not allowed"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
