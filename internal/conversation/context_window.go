package conversation

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/transcript"
)

// ContextSourceFile identifies a required source or an optional sidecar.
type ContextSourceFile struct {
	Path     string
	Required bool
}

// ReadVerifiedMessageWindow validates source stability around the callback.
// The callback must compare the messages with the caller's committed content.
func (idx *Index) ReadVerifiedMessageWindow(ctx context.Context, record Record, start, end int, loadRules string, visit func([]transcript.Message) error) (err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "conversation.context_read.failed", "component", "conversation", "concern", "conversation.load", "conversation_id", record.ID, "err", err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("read verified context: %w", err)
	}
	if visit == nil || start < 0 || end < start {
		return errors.New("invalid verified conversation message window")
	}
	options, known := LoadOptionsForRules(loadRules)
	if !known {
		return fmt.Errorf("unknown conversation loading rules %q", loadRules)
	}
	parser, err := idx.registry.Lookup(record.Provider)
	if err != nil {
		return err
	}
	if fresh, ok := parser.(FreshContextParser); ok {
		if err := fresh.ReadContextWindow(ctx, record.ArtifactPath, record.Selector, start, end, options, visit); err != nil {
			return fmt.Errorf("read fresh provider context: %w", err)
		}
		return nil
	}
	return WithStableContextSources(ctx, []ContextSourceFile{{Path: record.ArtifactPath, Required: true}}, func() error {
		messages, _, readErr := idx.readMessageWindow(ctx, record, start, end, options)
		if readErr != nil {
			return readErr
		}
		return visit(messages)
	})
}

// VisitContextWindow retains only the requested positions from a fresh stream.
func VisitContextWindow(ctx context.Context, stream iter.Seq2[transcript.Message, error], start, end int, visit func([]transcript.Message) error) error {
	if start < 0 || end < start || visit == nil {
		return errors.New("invalid provider context window")
	}
	var messages []transcript.Message
	position := 0
	if end > start {
		for message, err := range stream {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return &contextReadError{operation: "read context stream", cause: err}
			}
			if position >= start {
				messages = append(messages, message)
			}
			position++
			if position >= end {
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return &contextReadError{operation: "visit context window", cause: err}
	}
	return visit(messages)
}

// WithStableContextSources rejects replacement, edits, loss and sidecar changes
// during the source read and the caller's content verification.
func WithStableContextSources(ctx context.Context, sources []ContextSourceFile, read func() error) error {
	if len(sources) == 0 || read == nil {
		return errors.New("verified context requires source files and a reader")
	}
	before := make([]os.FileInfo, len(sources))
	for index, source := range sources {
		info, err := contextSourceInfo(source)
		if err != nil {
			return err
		}
		before[index] = info
	}
	if err := ctx.Err(); err != nil {
		return &contextReadError{operation: "read context sources", cause: err}
	}
	if err := read(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return &contextReadError{operation: "verify context sources", cause: err}
	}
	for index, source := range sources {
		after, err := contextSourceInfo(source)
		if err != nil {
			return err
		}
		prior := before[index]
		if (prior == nil) != (after == nil) {
			return fmt.Errorf("context source presence changed: %s", source.Path)
		}
		if prior != nil && (!os.SameFile(prior, after) || prior.Size() != after.Size() || !prior.ModTime().Equal(after.ModTime())) {
			return fmt.Errorf("context source changed: %s", source.Path)
		}
	}
	return nil
}

func contextSourceInfo(source ContextSourceFile) (os.FileInfo, error) {
	info, err := os.Stat(source.Path)
	if errors.Is(err, os.ErrNotExist) && !source.Required {
		return nil, nil
	}
	if err != nil {
		return nil, &contextReadError{operation: "stat context source", cause: err}
	}
	return info, nil
}

type contextReadError struct {
	operation string
	cause     error
}

func (failure *contextReadError) Error() string {
	return failure.operation + ": " + failure.cause.Error()
}

func (failure *contextReadError) Unwrap() error {
	return failure.cause
}
