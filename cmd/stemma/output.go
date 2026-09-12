package main

import (
	"context"
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
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/term"
)

type commandOutput struct {
	mu                                sync.Mutex
	out                               io.Writer
	logger                            *slog.Logger
	progress                          *mpb.Progress
	bar                               *mpb.Bar
	interactive                       bool
	started                           time.Time
	stage                             atomic.Pointer[stageDisplay]
	level, format                     string
	quiet, verbose, debug, noProgress bool
}

type stageDisplay struct {
	label   string
	started time.Time
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
	terminal := false
	if file, ok := o.out.(*os.File); ok {
		terminal = term.IsTerminal(int(file.Fd())) && os.Getenv("TERM") != "dumb"
	}
	o.interactive = terminal && !o.noProgress && o.format == "text" && os.Getenv("CI") == "" && level <= slog.LevelInfo
	var handler slog.Handler
	replace := func(_ []string, attr slog.Attr) slog.Attr {
		if attr.Key == "stage" {
			return slog.Attr{}
		}
		return attr
	}
	if o.format == "json" {
		handler = slog.NewJSONHandler(o, &slog.HandlerOptions{Level: level})
	} else {
		handler = tint.NewTextHandler(o, &tint.Options{Level: level, NoColor: !terminal || os.Getenv("NO_COLOR") != "", TimeFormat: "15:04:05", ReplaceAttr: replace})
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
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progress != nil {
		o.bar.Abort(true)
		o.progress.Wait()
		o.progress, o.bar = nil, nil
	}
}

func (o *commandOutput) finish(err error) {
	o.stop()
	if o.started.IsZero() && o.format == "json" {
		o.logger = slog.New(slog.NewJSONHandler(o.out, nil))
	}
	if err != nil {
		o.logger.Error("Command failed", "error", err)
	} else if o.stage.Load() != nil {
		o.logger.Info("Completed", "elapsed", time.Since(o.started).Round(time.Millisecond))
	}
}

// Stop live rendering before stdout reports so terminal redraws cannot erase them.
type reportWriter struct {
	io.Writer
	output *commandOutput
}

func (w reportWriter) Write(data []byte) (int, error) {
	w.output.stop()
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
	stage := false
	label := record.Message
	read := func(attr slog.Attr) bool {
		if attr.Key == "stage" && attr.Value.Kind() == slog.KindBool {
			stage = attr.Value.Bool()
		}
		if attr.Key == "resource" || attr.Key == "destination" || attr.Key == "input" || attr.Key == "plugin" {
			label += " · " + attr.Value.String()
		}
		return true
	}
	for _, attr := range h.attrs {
		read(attr)
	}
	record.Attrs(read)
	if stage {
		o := h.output
		o.mu.Lock()
		o.stage.Store(&stageDisplay{label: label, started: time.Now()})
		if o.interactive {
			if o.progress == nil {
				o.progress = mpb.New(mpb.WithOutput(o.out), mpb.WithRefreshRate(100*time.Millisecond))
				o.bar = o.progress.AddSpinner(0, mpb.BarWidth(1), mpb.AppendDecorators(decor.Any(func(decor.Statistics) string {
					stage := o.stage.Load()
					return " " + stage.label + " · " + time.Since(stage.started).Round(time.Second).String()
				})))
			}
		}
		o.mu.Unlock()
	}
	return h.Handler.Handle(ctx, record)
}
