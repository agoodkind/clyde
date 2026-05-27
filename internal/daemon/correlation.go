package daemon

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog"
	"goodkind.io/gklog/correlation"
)

type correlationServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s correlationServerStream) Context() context.Context {
	return s.ctx
}

func daemonUnaryCorrelationInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		corr := clydeingress.FromIncomingMetadata(ctx).Child()
		ctx = daemonCorrelationContext(ctx, log, corr)
		started := daemonNow()
		attrs := []slog.Attr{
			slog.String("component", "daemon"),
			slog.String("method", info.FullMethod),
		}
		attrs = append(attrs, corr.Attrs()...)
		slogger.WithConcern(gklog.LoggerFromContext(ctx), slogger.ConcernDaemonRPCRequests).LogAttrs(ctx, slog.LevelInfo, "daemon.rpc.started", attrs...)
		resp, err := handler(ctx, req)
		logDaemonRPCCompleted(ctx, info.FullMethod, started, err)
		return resp, err
	}
}

func daemonStreamCorrelationInterceptor(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		corr := clydeingress.FromIncomingMetadata(stream.Context()).Child()
		ctx := daemonCorrelationContext(stream.Context(), log, corr)
		started := daemonNow()
		attrs := []slog.Attr{
			slog.String("component", "daemon"),
			slog.String("method", info.FullMethod),
		}
		attrs = append(attrs, corr.Attrs()...)
		slogger.WithConcern(gklog.LoggerFromContext(ctx), slogger.ConcernDaemonRPCStreams).LogAttrs(ctx, slog.LevelInfo, "daemon.rpc.stream_started", attrs...)
		err := handler(srv, correlationServerStream{ServerStream: stream, ctx: ctx})
		logDaemonRPCCompleted(ctx, info.FullMethod, started, err)
		return err
	}
}

func daemonCorrelationContext(ctx context.Context, log *slog.Logger, corr correlation.Context) context.Context {
	ctx = correlation.WithContext(ctx, corr)
	if log == nil {
		log = slog.Default()
	}
	return gklog.WithLogger(ctx, log)
}

func daemonDetachedCorrelationContext(parent context.Context, log *slog.Logger) context.Context {
	corr := correlation.FromContext(parent)
	if corr.TraceID == "" {
		corr = correlation.New("")
	} else {
		corr = corr.Child()
	}
	return daemonCorrelationContext(context.Background(), log, corr)
}

func logDaemonRPCCompleted(ctx context.Context, method string, started time.Time, err error) {
	code := status.Code(err).String()
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("component", "daemon"),
		slog.String("method", method),
		slog.String("status", code),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	attrs = append(attrs, correlation.AttrsFromContext(ctx)...)
	slogger.WithConcern(gklog.LoggerFromContext(ctx), slogger.ConcernDaemonRPCRequests).LogAttrs(ctx, level, "daemon.rpc.completed", attrs...)
}

func daemonUnaryClientCorrelationInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req any, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, _ = correlation.Ensure(ctx, "")
		return invoker(clydeingress.NewOutgoingContext(ctx), method, req, reply, cc, opts...)
	}
}

func daemonStreamClientCorrelationInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx, _ = correlation.Ensure(ctx, "")
		return streamer(clydeingress.NewOutgoingContext(ctx), desc, cc, method, opts...)
	}
}
