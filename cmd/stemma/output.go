package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/plugin"
)

type commandOutput struct {
	mu                                sync.Mutex
	out                               io.Writer
	logger                            *slog.Logger
	progress                          *terminalProgress
	interactive                       bool
	style                             textStyle
	started                           time.Time
	staged                            atomic.Bool
	reported                          bool
	level, format                     string
	quiet, verbose, debug, noProgress bool
}

func newCommandOutput(cmd *cobra.Command, out io.Writer) *commandOutput {
	o := &commandOutput{out: out}
	o.logger = slog.New(tint.NewTextHandler(out, &tint.Options{NoColor: true}))
	flags := cmd.PersistentFlags()
	flags.StringVar(&o.level, "log-level", "info", "Log level: debug, info, warn or error")
	flags.StringVar(&o.format, "log-format", "text", "Stderr log format: text or json")
	flags.BoolVarP(&o.quiet, "quiet", "q", false, "Show only warnings and errors on stderr")
	flags.BoolVarP(&o.verbose, "verbose", "v", false, "Show debug diagnostics")
	flags.BoolVarP(&o.debug, "debug", "d", false, "Show debug diagnostics (same as --verbose)")
	flags.BoolVar(&o.noProgress, "no-progress", false, "Use ordinary log lines without live terminal progress")
	cmd.MarkFlagsMutuallyExclusive("log-level", "quiet", "verbose", "debug")
	return o
}

func (o *commandOutput) start(cmd *cobra.Command) error {
	var level slog.Level
	switch strings.ToLower(o.level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("invalid log level %q: use debug, info, warn or error", o.level)
	}
	if o.quiet {
		level = slog.LevelWarn
	}
	if o.verbose || o.debug {
		level = slog.LevelDebug
	}
	if o.format != "text" && o.format != "json" {
		return fmt.Errorf("invalid log format %q: use text or json", o.format)
	}
	terminal := terminalOutput(o.out)
	o.style = newTextStyle(o.out)

	o.interactive = terminal && !o.noProgress && o.format == "text" && os.Getenv("CI") == "" && level <= slog.LevelInfo
	var handler slog.Handler
	replace := func(_ []string, attr slog.Attr) slog.Attr {
		if attr.Key == "stage" || attr.Key == "progress" || attr.Key == "progress_final" || attr.Key == "stage_result" {
			return slog.Attr{}
		}
		return attr
	}
	if o.format == "json" {
		handler = slog.NewJSONHandler(o, &slog.HandlerOptions{Level: level})
	} else {
		handler = tint.NewTextHandler(o, &tint.Options{Level: level, NoColor: !o.style.enabled, TimeFormat: "15:04:05", ReplaceAttr: replace})
	}
	o.logger = slog.New(&stageHandler{Handler: handler, output: o})
	o.started = time.Now()
	cmd.SetContext(plugin.WithLogger(cmd.Context(), o.logger))
	return nil
}

func (o *commandOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progress != nil {
		return o.progress.Write(data)
	}
	return o.out.Write(data)
}

func (o *commandOutput) stop() {
	o.endProgress(nil)
}

func (o *commandOutput) finish(err error) {
	o.endProgress(err)
	o.interactive = false
	if o.started.IsZero() && o.format == "json" {
		o.logger = slog.New(slog.NewJSONHandler(o.out, nil))
	}
	switch {
	case errors.Is(err, context.Canceled):
		o.logger.Warn("Interrupted")
	case err != nil:
		o.logger.Error("Command failed", "error", err)
	case o.staged.Load() && !o.reported:
		o.logger.Info("Completed", "elapsed", time.Since(o.started).Round(time.Millisecond))
	}
}

// Stop live rendering before stdout reports so terminal redraws cannot erase them.
type reportWriter struct {
	io.Writer

	output *commandOutput
}

func (w reportWriter) Write(data []byte) (int, error) {
	if terminalOutput(w.Writer) {
		w.output.stop()
	}
	return w.Writer.Write(data)
}

type stageHandler struct {
	slog.Handler

	output *commandOutput
	attrs  []slog.Attr
}

func (h *stageHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &stageHandler{Handler: h.Handler.WithAttrs(attrs), output: h.output, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *stageHandler) WithGroup(name string) slog.Handler {
	return &stageHandler{Handler: h.Handler.WithGroup(name), output: h.output, attrs: h.attrs}
}

func (h *stageHandler) Handle(ctx context.Context, record slog.Record) error {
	a := readActivity(record, h.attrs)
	o := h.output
	if a.stage {
		o.staged.Store(true)
	}
	if o.interactive && (a.stage || a.progress || a.status || record.Level >= slog.LevelWarn && a.scope != "") {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.progress == nil {
			o.progress = newTerminalProgress(o.out)
		}
		if a.stage || a.progress || a.status {
			o.progress.update(a)
		} else {
			message := a.label
			if a.err != "" {
				message += ": " + a.err
			}
			outcome := "warning"
			if record.Level >= slog.LevelError {
				outcome = "failed"
			}
			o.progress.note(a.scope, message, outcome)
		}
		return nil
	}
	if a.progress && !a.final {
		record.Level = slog.LevelDebug
		if !h.Enabled(ctx, record.Level) {
			return nil
		}
	}
	return h.Handler.Handle(ctx, record)
}

func (o *commandOutput) resourceDone(out io.Writer, format, method string, resource engine.ResourceReport) error {
	details := resource.Error != ""
	for _, destination := range resource.Destinations {
		details = details || destination.Error != "" || len(destination.Changes) > 0
	}
	if o.interactive {
		o.mu.Lock()
		if o.progress != nil {
			for _, destination := range resource.Destinations {
				for _, change := range destination.Changes {
					o.progress.note(resourceName(resource), destination.Name+": "+change.Action+" "+change.Field, "detail")
				}
			}
			o.progress.complete(resourceName(resource), resourceStatus(method, resource), resource.Error != "")
		}
		o.mu.Unlock()
		if terminalOutput(out) {
			return nil
		}
	}
	if format != "json" && details {
		return printResource(out, method, resource)
	}
	return nil
}

func (o *commandOutput) report(out io.Writer, format, method string, report engine.Report, runErr error) error {
	o.endProgress(runErr)
	o.reported = true
	if format == "json" {
		return writeJSON(out, report)
	}
	if errors.Is(runErr, context.Canceled) {
		return nil
	}
	return printSummary(out, method, report)
}

func (o *commandOutput) endProgress(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progress != nil {
		outcome := ""
		if err != nil {
			outcome = "not completed"
		}
		if errors.Is(err, context.Canceled) {
			outcome = "interrupted"
		}
		o.progress.stop(outcome)
		o.progress = nil
	}
}
