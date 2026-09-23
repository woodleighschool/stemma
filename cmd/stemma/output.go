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

	"github.com/fatih/color"
	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/reconcile"
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
	failed                            int
	level, format                     string
	quiet, verbose, debug, noProgress bool
}

func newCommandOutput(cmd *cobra.Command, out io.Writer) *commandOutput {
	o := &commandOutput{out: out}
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
	if o.started.IsZero() {
		o.style = newTextStyle(o.out)
		o.logger = slog.New(slog.NewJSONHandler(o.out, nil))
	}
	switch {
	case err == nil:
		if o.staged.Load() && !o.reported {
			o.logger.Info("Completed", "elapsed", time.Since(o.started).Round(time.Millisecond))
		}
	case o.format == "json" && errors.Is(err, context.Canceled):
		o.logger.Warn("Interrupted")
	case o.format == "json":
		o.logger.Error("Command failed", "error", err)
	case errors.Is(err, context.Canceled):
		_, _ = fmt.Fprintln(o.out, o.style.paint("Interrupted", color.FgHiYellow))
	default:
		_, _ = io.WriteString(o.out, o.failure(err))
	}
}

// failure renders the final error. Failures the resource reports already showed
// are counted rather than repeated.
func (o *commandOutput) failure(err error) string {
	label := o.style.paint("Error:", color.Bold, color.FgHiRed)
	var text strings.Builder
	var write func(error)
	write = func(err error) {
		if _, reported := err.(engine.ReportedError); reported && o.failed > 0 { //nolint:errorlint // Wrapped errors may contain unreported failures.
			return
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				write(child)
			}
			return
		}
		_, _ = fmt.Fprintf(&text, "%s %s\n", label, strings.Join(errorLines(err.Error()), "\n"))
	}
	write(err)
	if o.failed > 0 {
		noun := "resources"
		if o.failed == 1 {
			noun = "resource"
		}
		_, _ = fmt.Fprintf(&text, "%s %d %s failed\n", label, o.failed, noun)
	}
	return text.String()
}

// errorLines splits error text for display, indenting what follows the first
// line. Joined errors and schema violations carry newlines and tabs.
func errorLines(text string) []string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(cleanLine(strings.ReplaceAll(line, "\t", "  ")), " ")
		if line == "" {
			continue
		}
		if len(lines) > 0 {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	return lines
}

// Stop live rendering before stdout reports so terminal redraws cannot erase them.
type reportWriter struct {
	io.Writer

	output *commandOutput
}

func (w reportWriter) Write(data []byte) (int, error) {
	w.output.reported = true
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
	if o.interactive && o.live(a, record.Level) {
		return nil
	}
	// Text logs name a stage once, when it starts. Its result and interim
	// progress are debug detail; JSON logs keep every stage result.
	if a.progress && !a.final || a.status && o.format != "json" {
		record.Level = slog.LevelDebug
		if !h.Enabled(ctx, record.Level) {
			return nil
		}
	}
	return h.Handler.Handle(ctx, record)
}

// live renders a record in the terminal tree. Only a stage opens the tree;
// progress, results and scoped warnings join it while it is open.
func (o *commandOutput) live(a activity, level slog.Level) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progress == nil {
		if !a.stage {
			return false
		}
		o.progress = newTerminalProgress(o.out)
	}
	switch {
	case a.stage || a.progress || a.status:
		o.progress.update(a)
	case level >= slog.LevelWarn && a.scope != "":
		message, outcome := a.label, "warning"
		if a.err != "" {
			message += ": " + a.err
		}
		if level >= slog.LevelError {
			outcome = "failed"
		}
		o.progress.note(a.scope, message, outcome)
	default:
		return false
	}
	return true
}

func (o *commandOutput) resourceDone(out io.Writer, asJSON bool, method string, resource engine.ResourceReport) (err error) {
	failure := failureLines(resource)
	defer func() {
		if err == nil && len(failure) > 0 && len(resource.BlockedBy) == 0 {
			o.failed++
		}
	}()
	details := len(failure) > 0
	for _, destination := range resource.Destinations {
		details = details || len(destination.Changes) > 0
	}
	shown := false
	if o.interactive {
		o.mu.Lock()
		if o.progress != nil {
			name := resourceName(resource)
			for _, destination := range resource.Destinations {
				for _, change := range destination.Changes {
					o.progress.note(name, destination.Name+": "+change.Action+" "+change.Field, "detail")
				}
			}
			shown = o.progress.complete(name, resourceStatus(method, resource), len(failure) > 0, failure)
		}
		o.mu.Unlock()
		if shown && terminalOutput(out) {
			return nil
		}
	}
	// A created icon names the presentation the host chose and missing artwork
	// needs a committed file, so an icon run always shows both.
	reported := method == "icon" && resource.Icon != "" && resource.Icon != "unchanged" && resource.Icon != "no icon declared"
	switch {
	case !asJSON && (details || method == "signature" || reported):
		return printResource(out, method, resource)
	case asJSON && len(failure) > 0 && !shown && o.format == "json":
		o.logger.Error("Resource "+resourceStatus(method, resource), "resource", resourceName(resource), "error", resource.Error)
	case asJSON && len(failure) > 0 && !shown:
		return printResource(o, method, resource)
	}
	return nil
}

// failureLines lists what failed in a resource, naming each failed destination.
func failureLines(resource engine.ResourceReport) []string {
	var lines []string
	for _, destination := range resource.Destinations {
		if destination.Error != "" {
			lines = append(lines, errorLines(destination.Name+": "+destination.Error)...)
		}
	}
	if len(lines) == 0 {
		lines = errorLines(resource.Error)
	}
	return lines
}

func (o *commandOutput) report(out io.Writer, asJSON bool, method string, report engine.Report, runErr error) error {
	o.endProgress(runErr)
	o.reported = true
	if asJSON {
		return writeJSON(out, report)
	}
	if errors.Is(runErr, context.Canceled) {
		return nil
	}
	return printSummary(out, method, report)
}

func (o *commandOutput) reconciled(out io.Writer, asJSON bool, report reconcile.Report, runErr error) error {
	o.endProgress(runErr)
	o.reported = true
	if asJSON {
		return writeJSON(out, report)
	}
	if errors.Is(runErr, context.Canceled) {
		return nil
	}
	return printReconcile(out, report)
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
