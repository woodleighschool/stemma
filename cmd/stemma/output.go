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

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/changes"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/reconcile"
	"github.com/woodleighschool/stemma/plugin"
)

// commandOutput owns what a command shows. Human reports stream to stdout as
// each resource completes; in a terminal a live tree of unfinished work sits
// below them on stderr. JSON reports are one document written at the end.
type commandOutput struct {
	mu                 sync.Mutex
	out, errOut        io.Writer
	outStyle, errStyle textStyle
	interactive        bool
	progress           *terminalProgress
	asJSON, all        bool
	reconciling        bool
	// pathOnly commands print one path on stdout and report nothing else.
	pathOnly bool
	// JSON reports carry the warnings raised while they ran.
	warnings []string
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
	o.asJSON, _ = cmd.Flags().GetBool("json")
	o.all, _ = cmd.Flags().GetBool("all")
	switch cmd.Name() {
	case "operations", "schema":
		o.asJSON = true
	case "validate":
		o.asJSON, _ = cmd.Flags().GetBool("resolved")
	case "reconcile":
		o.reconciling = true
	case "artifact":
		o.pathOnly = true
	}
	// Report blocks print above the live tree, so both streams must share the
	// terminal; a path alone is written after the tree is gone.
	o.interactive = (o.pathOnly || terminalOutput(o.out)) && terminalOutput(o.errOut) && os.Getenv("CI") == ""
	cmd.SetContext(plugin.WithLogger(cmd.Context(), slog.New(&activityHandler{output: o})))
	return nil
}

func (o *commandOutput) stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.interactive = false
	if o.progress != nil {
		o.progress.stop()
		o.progress = nil
	}
}

// emit writes persistent report text to stdout, above the live tree while it
// is on screen. Callers hold o.mu.
func (o *commandOutput) emit(text string) error {
	if o.progress != nil && o.progress.running() {
		o.progress.print(text)
		return nil
	}
	_, err := io.WriteString(o.out, text)
	return err
}

// notice writes a diagnostic line to stderr, above the live tree while it is
// on screen. Callers hold o.mu.
func (o *commandOutput) notice(line string) {
	if o.progress != nil && o.progress.running() {
		o.progress.print(line)
		return
	}
	_, _ = io.WriteString(o.errOut, line)
}

func (o *commandOutput) finish(err error) {
	o.stop()
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, warning := range o.warnings {
		o.notice(o.warning(warning))
	}
	o.warnings = nil
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		_, _ = fmt.Fprintln(o.errOut, o.errStyle.paint("Interrupted.", color.FgHiYellow))
	default:
		text := commandError(err)
		if o.pathOnly {
			// No report shows a failed resource, so the error must.
			text = failureText(err)
		}
		if text != "" {
			_, _ = fmt.Fprintln(o.errOut, o.errStyle.paint("Error:", color.Bold, color.FgHiRed)+" "+strings.Join(errorLines(text), "\n"))
		}
	}
}

func (o *commandOutput) warning(text string) string {
	return o.errStyle.paint("Warning:", color.FgHiYellow) + " " + changes.Text(text) + "\n"
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

// activityHandler adapts operation telemetry to the live tree. Stage records
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
	if !o.interactive || !a.stage && !a.progress && !a.status {
		return nil
	}
	if o.progress == nil {
		if !a.stage {
			return nil
		}
		o.progress = newTerminalProgress(o.errOut)
	}
	o.progress.update(a)
	return nil
}

// resourceDone streams a finished resource's report block. In a terminal, a
// resource the report leaves out still leaves its outcome line in place of its
// tree. Reconcile streams what it applies; proposal verification is
// summarised in the pull request.
func (o *commandOutput) resourceDone(method string, resource engine.ResourceReport) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progress != nil {
		o.progress.complete(resourceName(resource))
	}
	switch {
	case o.asJSON || o.pathOnly:
		return nil
	case o.reconciling && method != "apply":
		// Proposals report what the lookup found and what their checks showed;
		// a terminal still marks each resource the lookup finishes.
		if method == "update" && o.interactive {
			return o.emit(resourceHeading(o.outStyle, method, resource))
		}
		return nil
	case o.all || selected(method, resource):
		return o.emit(renderResource(o.outStyle, method, resource))
	case o.interactive:
		return o.emit(resourceHeading(o.outStyle, method, resource))
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

// proposalDone streams a proposal branch's outcome. Without --all, an
// unchanged branch shows only in a terminal.
func (o *commandOutput) proposalDone(proposal reconcile.Proposal) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.asJSON || !o.all && !o.interactive && proposal.Action == "unchanged" && proposal.Error == "" {
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
	o.stop()
	report.Warnings = append(report.Warnings, o.takeWarnings()...)
	if report.Resources == nil && report.LockChanged == nil && len(report.Warnings) == 0 {
		return nil
	}
	if o.asJSON {
		return writeJSON(out, selectReport(report, method, o.all))
	}
	return printReportEnd(out, method, report, runErr)
}

func (o *commandOutput) reconciled(out io.Writer, report reconcile.Report, runErr error) error {
	o.stop()
	report.Warnings = append(report.Warnings, o.takeWarnings()...)
	if report.Head == "" && len(report.Warnings) == 0 {
		return nil
	}
	if !o.asJSON {
		return printReconcileEnd(out, report, runErr)
	}
	if report.Apply != nil && report.Apply.Report != nil {
		apply := *report.Apply
		selected := selectReport(*apply.Report, "apply", o.all)
		apply.Report = &selected
		report.Apply = &apply
	}
	if report.Update != nil {
		update := reconcile.Update{Error: report.Update.Error}
		for _, proposal := range report.Update.Proposals {
			if !o.all && proposal.Action == "unchanged" && proposal.Error == "" {
				continue
			}
			if proposal.Plan != nil {
				selected := selectReport(*proposal.Plan, "plan", o.all)
				proposal.Plan = &selected
			}
			update.Proposals = append(update.Proposals, proposal)
		}
		report.Update = &update
	}
	return writeJSON(out, report)
}
