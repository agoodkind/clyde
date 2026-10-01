package conversation

import (
	"context"
	"errors"
	"fmt"
	"iter"
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
	if visit == nil {
		return errors.New("invalid verified conversation message window")
	}
	_, err = idx.ReadVerifiedMessageWindows(ctx, record, []ContextMessageWindow{{Start: start, End: end}}, loadRules, func(windows [][]transcript.Message) error {
		return visit(windows[0])
	})
	return err
}

// VisitContextWindow retains only the requested positions from a fresh stream.
func VisitContextWindow(ctx context.Context, stream iter.Seq2[transcript.Message, error], start, end int, visit func([]transcript.Message) error) error {
	if visit == nil {
		return errors.New("invalid provider context window")
	}
	_, err := VisitContextWindows(ctx, stream, []ContextMessageWindow{{Start: start, End: end}}, func(windows [][]transcript.Message) error {
		return visit(windows[0])
	})
	return err
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
		return &ContextReadError{Operation: "read context sources", Cause: err}
	}
	if err := read(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return &ContextReadError{Operation: "verify context sources", Cause: err}
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
		return nil, &ContextReadError{Operation: "stat context source", Cause: err}
	}
	return info, nil
}

// ContextReadError wraps a source failure without logging it.
type ContextReadError struct {
	Operation string
	Cause     error
}

// Error returns the operation followed by the original error text.
func (failure *ContextReadError) Error() string {
	return failure.Operation + ": " + failure.Cause.Error()
}

// Unwrap returns the original error for [errors.Is] and [errors.As].
func (failure *ContextReadError) Unwrap() error {
	return failure.Cause
}
