package plugin

import (
	"context"
	"log/slog"
)

type loggerKey struct{}

// WithLogger attaches the caller's logger to an operation and its nested work.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// Logger returns the operation logger, or a silent logger for library callers.
// Log only deliberate diagnostics; configuration, bindings and HTTP bodies may
// contain credentials and must never be logged.
func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.New(slog.DiscardHandler)
}

// Stage reports the current activity. It does not imply completion or success.
func Stage(ctx context.Context, message string) {
	Logger(ctx).InfoContext(ctx, message, "stage", true)
}
