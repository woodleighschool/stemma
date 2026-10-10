package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/changes"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/reconcile"
	"github.com/woodleighschool/stemma/plugin"
)

// commandOutput owns what a command shows. Human reports stream to stdout as
// each resource completes; in a terminal a live region of unfinished work sits
// below them on stderr. JSON reports are one document written at the end.
type commandOutput struct {
	activityEnabled      bool
	mu                   sync.Mutex
	out, errOut          io.Writer
	outStyle, errStyle   textStyle
	interactive          bool
	progress             *terminalProgress
	asJSON, all, details bool
	reconciling          bool
	selectors            []string
	// resultOnly commands print their result without a resource report.
	resultOnly bool
	ctx        context.Context
	// JSON reports carry the warnings raised while they ran.
	warnings []string
	// Reconcile can visit the same resource in several phases.
	notices map[string][]plugin.Notice
}

// finalWriter clears live progress before a command writes its final output.
type finalWriter struct {
	io.Writer

	output *commandOutput
}

func (w finalWriter) Write(data []byte) (int, error) { w.output.stop(); return w.Writer.Write(data) }

func newCommandOutput(out, errOut io.Writer) *commandOutput {
	return &commandOutput{out: out, errOut: errOut, outStyle: newTextStyle(out), errStyle: newTextStyle(errOut)}
}

func (o *commandOutput) start(cmd *cobra.Command) error {
	o.ctx = cmd.Context()
	o.asJSON, _ = cmd.Flags().GetBool("json")
	o.all, _ = cmd.Flags().GetBool("all")
	o.details, _ = cmd.Flags().GetBool("details")
	switch cmd.Name() {
	case "schema":
		o.asJSON = true
	case "validate":
		o.asJSON, _ = cmd.Flags().GetBool("resolved")
	case "reconcile":
		o.reconciling = true
	case "artifact", "inspect":
		o.resultOnly = true
	}
	// Activity belongs to stderr regardless of where stdout is redirected.
	noProgress, _ := cmd.Flags().GetBool("no-progress")
	o.activityEnabled = cmd.Name() != "mcp" && !o.asJSON && !noProgress
	o.interactive = o.activityEnabled && terminalOutput(o.errOut) && os.Getenv("CI") == ""
	cmd.SetContext(plugin.WithLogger(cmd.Context(), slog.New(&activityHandler{output: o})))
	return nil
}

func (o *commandOutput) stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.interactive = false
	o.activityEnabled = false
	if o.progress != nil {
		o.progress.stop()
		o.progress = nil
	}
}

// emit writes persistent report text to stdout, above the live region while it
// is on screen. Callers hold o.mu.
func (o *commandOutput) emit(text string) error {
	if o.progress != nil {
		return o.progress.write(o.out, text)
	}
	_, err := io.WriteString(o.out, text)
	return err
}

func (o *commandOutput) reconcilePhase(method string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.asJSON {
		return nil
	}
	title, prefix := "Applying reviewed branch", ""
	if method == "update" {
		title, prefix = "Proposing updates", "\n"
	}
	return o.emit(prefix + o.outStyle.paint(title, color.Bold) + "\n\n")
}

// notice writes a diagnostic line to stderr, above the live region while it is
// on screen. Callers hold o.mu.
func (o *commandOutput) notice(line string) {
	if o.progress != nil {
		_ = o.progress.write(o.errOut, line)
		return
	}
	_, _ = io.WriteString(o.errOut, line)
}

func (o *commandOutput) finish(err error) {
	if o.ctx != nil && errors.Is(context.Cause(o.ctx), errInterrupted) {
		err = errInterrupted
	}
	o.stop()
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, warning := range o.warnings {
		o.notice(o.warning(warning))
	}
	o.warnings = nil
	switch {
	case err == nil:
	case errors.Is(err, errInterrupted):
		_, _ = fmt.Fprintln(o.errOut, o.errStyle.paint("– Interrupted.", color.FgHiYellow))
	default:
		text := commandError(err)
		if o.resultOnly {
			// No report shows a failed resource, so the error must.
			text = failureText(err)
		}
		if text == "" {
			text = strings.SplitN(failureText(err), "\n", 2)[0]
		}
		if text != "" {
			var message strings.Builder
			writeError(&message, o.errStyle, "", text)
			_, _ = io.WriteString(o.errOut, message.String())
		}
	}
}

func (o *commandOutput) warning(text string) string {
	return o.errStyle.paint("!", color.FgHiYellow) + " " + changes.Text(text) + "\n"
}

func (o *commandOutput) resourceNotice(resource string, notice plugin.Notice) string {
	marker := o.errStyle.paint("!", color.FgHiYellow)
	if notice.Level == "info" {
		marker = o.errStyle.paint("i", color.Faint)
	}
	text := marker + " " + changes.Text(resource) + ": " + changes.Text(notice.Message) + "\n"
	if notice.Hint != "" {
		text += "  " + changes.Text(notice.Hint) + "\n"
	}
	return text
}

// commandError is the part of a failure the report did not show. Each failed
// resource is in the report with its totals, and a reconcile report holds each
// failed phase and proposal; the exit status alone says the command failed.
func commandError(err error) string {
	if err == reconcile.ErrFailed || err == engine.ErrPluginsFailed { //nolint:errorlint // Joined with anything else, the report was not written.
		return ""
	}
	if err = engine.Unreported(err); err == nil {
		return ""
	}
	return err.Error()
}

// failureText names each failed resource the way reports do.
func failureText(err error) string {
	if failure, ok := err.(engine.ResourceError); ok { //nolint:errorlint // Only a direct resource failure has a key to shorten.
		return keyName(failure.Resource) + ": " + failure.Err.Error()
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var lines []string
		for _, child := range joined.Unwrap() {
			lines = append(lines, failureText(child))
		}
		return strings.Join(lines, "\n")
	}
	return err.Error()
}

func errorLines(text string) []string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		line = changes.Text(strings.TrimRight(strings.ReplaceAll(line, "\t", "  "), " "))
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

// activityHandler adapts operation telemetry to the live region. Stage records
// are never logs; warnings reach stderr, or the JSON report while one runs.
type activityHandler struct {
	output *commandOutput
	attrs  []slog.Attr
}

func (*activityHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *activityHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &activityHandler{output: h.output, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *activityHandler) WithGroup(_ string) slog.Handler { return h }

func (h *activityHandler) Handle(_ context.Context, record slog.Record) error {
	a := readActivity(record, h.attrs)
	o := h.output
	o.mu.Lock()
	defer o.mu.Unlock()
	var maintenance bool
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "cache_maintenance" {
			maintenance = attr.Value.Bool()
		}
		return true
	})
	if maintenance {
		o.notice(o.errStyle.paint("i", color.Faint) + " " + changes.Text(record.Message) + "\n")
		return nil
	}
	if record.Level >= slog.LevelWarn && !a.status {
		note := a.label
		if a.scope != "" {
			note = a.scope + ": " + note
		}
		if a.err != "" {
			note += ": " + a.err
		}
		if o.asJSON {
			o.warnings = append(o.warnings, note)
		} else {
			o.notice(o.warning(note))
		}
		return nil
	}
	if !o.activityEnabled && !o.interactive || !a.stage && !a.progress && !a.status {
		return nil
	}
	if o.progress == nil {
		if !a.stage {
			return nil
		}
		o.progress = newTerminalProgress(o.errOut)
		if !o.interactive {
			o.progress.startPlain()
		}
	}
	o.progress.update(a)
	return nil
}

// resourceDone clears a resource's activity and streams its selected outcome.
// Reconcile reports proposal verification through proposal outcomes.
func (o *commandOutput) resourceDone(method string, resource engine.ResourceReport) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progress != nil {
		o.progress.complete(resourceName(resource))
	}
	if !o.asJSON {
		if o.notices == nil {
			o.notices = map[string][]plugin.Notice{}
		}
		key := resource.Key
		if key == "" {
			key = resourceName(resource)
		}
		for _, notice := range resource.Notices {
			if !slices.Contains(o.notices[key], notice) {
				o.notice(o.resourceNotice(resourceName(resource), notice))
				o.notices[key] = append(o.notices[key], notice)
			}
		}
	}
	full := o.all || slices.Contains(o.selectors, resource.Key) || slices.Contains(o.selectors, resourceName(resource)) || slices.Contains(o.selectors, resource.Name)
	switch {
	case o.asJSON || o.resultOnly:
		return nil
	case o.reconciling && method != "apply":
		// Proposal outcomes carry the lookup and preparation results.
		return nil
	case full || selected(method, resource):
		return o.emit(renderResourceDetail(o.outStyle, method, resource, o.details || method == "prepare" && full))
	}
	return nil
}

// applyDone streams the reviewed commit's publication outcome.
func (o *commandOutput) applyDone(report reconcile.Report) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.asJSON {
		return nil
	}
	return o.emit(renderReviewed(o.outStyle, report))
}

// proposalDone streams selected proposal outcomes.
func (o *commandOutput) proposalDone(proposal reconcile.Proposal) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.asJSON || !o.all && proposal.Action == "unchanged" && proposal.Error == "" {
		return nil
	}
	return o.emit(renderProposal(o.outStyle, proposal))
}

func (o *commandOutput) takeWarnings() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	warnings := o.warnings
	o.warnings = nil
	return warnings
}

// report ends an outcome command: the JSON document, or the human summary
// below the blocks already streamed.
func (o *commandOutput) report(out io.Writer, method string, report engine.Report, runErr error) error {
	if o.ctx != nil && errors.Is(context.Cause(o.ctx), errInterrupted) {
		runErr = errInterrupted
	}
	o.stop()
	report.Warnings = append(report.Warnings, o.takeWarnings()...)
	if report.Resources == nil && report.LockChanged == nil && len(report.Warnings) == 0 {
		return nil
	}
	if o.asJSON {
		return writeJSON(out, report)
	}
	return printReportEnd(out, method, report, runErr)
}

func (o *commandOutput) reconciled(out io.Writer, report reconcile.Report, runErr error) error {
	if o.ctx != nil && errors.Is(context.Cause(o.ctx), errInterrupted) {
		runErr = errInterrupted
	}
	o.stop()
	report.Warnings = append(report.Warnings, o.takeWarnings()...)
	if report.Head == "" && len(report.Warnings) == 0 {
		return nil
	}
	if !o.asJSON {
		return printReconcileEnd(out, report, runErr)
	}
	return writeJSON(out, report)
}
