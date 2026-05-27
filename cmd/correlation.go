package cmd

import (
	"context"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/util"
	"goodkind.io/gklog"
	"goodkind.io/gklog/correlation"
)

func newCommandContext(operation string) context.Context {
	requestID := "cli-" + util.GenerateUUID()
	if requestID == "cli-" {
		requestID = "cli-" + string(correlation.NewTraceID())
	}
	ctx, _ := correlation.Ensure(context.Background(), requestID)
	return gklog.WithLogger(ctx, slog.Default().With(
		"component", "cli",
		"operation", strings.TrimSpace(operation),
	))
}

func childCommandContext(parent context.Context, operation string) context.Context {
	corr := correlation.FromContext(parent)
	if corr.TraceID == "" {
		return newCommandContext(operation)
	}
	ctx := correlation.WithContext(parent, corr.Child())
	return gklog.WithLogger(ctx, gklog.LoggerFromContext(parent).With(
		"operation", strings.TrimSpace(operation),
	))
}
