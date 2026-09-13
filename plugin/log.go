package plugin

import (
	"context"
	"log/slog"
	"sync"
	"time"
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

// Stage starts an operation and returns its completion function. Call the function
// with the operation error; optional attributes describe its measured result.
// Nested stages finish independently of their enclosing operation. The first
// completion call wins; subsequent calls have no effect.
func Stage(ctx context.Context, message string) func(error, ...any) {
	logger := Logger(ctx)
	started := time.Now()
	logger.InfoContext(ctx, message, "stage", true)
	var once sync.Once
	return func(err error, attrs ...any) {
		once.Do(func() {
			args := []any{"stage_result", true, "elapsed", time.Since(started).Round(time.Millisecond)}
			if err != nil {
				args = append(args, "error", err)
			}
			logger.InfoContext(ctx, message, append(args, attrs...)...)
		})
	}
}
