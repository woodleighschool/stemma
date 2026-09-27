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

// Stage starts an operation and returns its completion function. Attributes
// describe the operation; call the function with the operation error and
// optional attributes that describe its result. Progress displays show a
// [Detail] beside the operation: its subject while it runs, then its outcome.
// Nested stages finish independently of their enclosing operation. The first
// completion call wins; subsequent calls have no effect.
func Stage(ctx context.Context, message string, attrs ...any) func(error, ...any) {
	logger := Logger(ctx)
	started := time.Now()
	logger.InfoContext(ctx, message, append([]any{"stage", true}, attrs...)...)
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

// Detail is short text shown beside a stage: the file, repository or host it
// works on, or the version, size or count it found. It must not carry
// credentials, query strings or response bodies.
func Detail(text string) slog.Attr { return slog.String("detail", text) }
