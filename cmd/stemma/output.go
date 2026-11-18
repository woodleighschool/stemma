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

// commandOutput owns reports and activity. Interactive output shares one
// terminal writer; redirected reports use stdout. JSON is one final document.
type commandOutput struct {
	activityEnabled bool
	mu              sync.Mutex
	out, errOut     io.Writer
	interactive     bool
	progress        *terminalProgress
	interrupt       func()
	asJSON          bool
	reconciling     bool
	// namedInputs identifies resources that need an input scope in their report.
	namedInputs map[string]bool
	streamed    bool
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

func (w finalWriter) Write(data []byte) (int, error) {
	if err := w.output.stop(); err != nil {
		return 0, err
	}
	return w.Writer.Write(data)
}

func newCommandOutput(out, errOut io.Writer) *commandOutput {
	return &commandOutput{out: out, errOut: errOut}
}

func (o *commandOutput) start(cmd *cobra.Command) error {
	o.ctx = cmd.Context()
	o.asJSON, _ = cmd.Flags().GetBool("json")
	switch cmd.Name() {
	case "schema":
		o.asJSON = true
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

func (o *commandOutput) stop() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.interactive = false
	o.activityEnabled = false
	if o.progress != nil {
		err := o.progress.stop()
		o.progress = nil
		return err
	}
	return nil
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

func (o *commandOutput) block(text string) error {
	o.streamed = true
	return o.emit(text)
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

func (o *commandOutput) finish(err error) error {
	if o.ctx != nil && errors.Is(context.Cause(o.ctx), errInterrupted) {
		err = errInterrupted
	}
	if stopErr := o.stop(); stopErr != nil {
		err = errors.Join(err, stopErr)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, warning := range o.warnings {
		o.notice(o.warning(warning))
	}
	o.warnings = nil
	switch {
	case err == nil:
	case errors.Is(err, errInterrupted):
		_, _ = fmt.Fprintln(o.errOut, newTextStyle(o.errOut).paint("– Interrupted.", color.FgHiYellow))
	default:
		text := commandError(err)
		if o.resultOnly {
			// No report shows a failed resource, so the error must.
			text = failureText(err)
		}
		if text != "" {
			var message strings.Builder
			writeError(&message, newTextStyle(o.errOut), "", text)
			_, _ = io.WriteString(o.errOut, message.String())
		}
	}
	return err
}

func (o *commandOutput) warning(message string) string {
	var text strings.Builder
	newTextStyle(o.errOut).writeHeading(&text, "", changes.Line{{Role: changes.Notable, Text: "!"}, {Role: changes.Strong, Text: " Warning"}}, nil)
	newTextStyle(o.errOut).writeLine(&text, "  ", changes.Line{{Role: changes.Plain, Text: changes.Text(message)}})
	return text.String()
}

func (o *commandOutput) resourceNotice(resource string, notice plugin.Notice) string {
	marker, role := "!", changes.Notable
	if notice.Level == "info" {
		marker, role = "i", changes.Old
	}
	var text strings.Builder
	newTextStyle(o.errOut).writeHeading(&text, "", changes.Line{{Role: role, Text: marker}, {Role: changes.Strong, Text: " " + changes.Text(resource)}}, nil)
	newTextStyle(o.errOut).writeLine(&text, "  ", changes.Line{{Role: changes.Plain, Text: changes.Text(notice.Message)}})
	if notice.Hint != "" {
		newTextStyle(o.errOut).writeLine(&text, "  ", changes.Line{{Role: changes.Plain, Text: changes.Text(notice.Hint)}})
	}
	return text.String()
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
		o.progress = newTerminalProgress(o.ctx, o.errOut, o.interactive, o.interrupt)
	}
	o.progress.update(a)
	return nil
}

func (o *commandOutput) started(selection engine.Selection) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.namedInputs == nil {
		o.namedInputs = map[string]bool{}
	}
	for _, name := range selection.NamedInputs {
		o.namedInputs[name] = true
	}
}

// resourceDone clears a resource's activity and streams its rows.
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
	switch {
	case o.asJSON || o.resultOnly:
		return nil
	case o.reconciling && method != "apply":
		// Proposal outcomes carry the lookup and preparation results.
		return nil
	}
	return o.block(renderRows(newTextStyle(o.out), resourceName(resource), resource.Status, resourceRows(method, resource, o.namedInputs[resourceName(resource)])))
}

// applyDone streams the reviewed commit's publication outcome.
func (o *commandOutput) applyDone(report reconcile.Report) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.asJSON {
		return nil
	}
	head, row := reviewedRow(report)
	status := engine.StatusChanged
	if report.Apply.Failed() {
		status = engine.StatusFailed
	}
	return o.block(renderRows(newTextStyle(o.out), head, status, []changes.Row{row}))
}

// proposalDone streams selected proposal outcomes.
func (o *commandOutput) proposalDone(proposal reconcile.Proposal) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.asJSON {
		return nil
	}
	status := engine.StatusChanged
	if proposal.Error != "" || proposal.Action == "blocked" {
		status = engine.StatusFailed
	}
	return o.block(renderRows(newTextStyle(o.out), proposal.Name, status, []changes.Row{proposalRow(proposal)}))
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
func (o *commandOutput) report(out io.Writer, method string, report engine.Report) error {
	if err := o.stop(); err != nil {
		return err
	}
	report.Warnings = append(report.Warnings, o.takeWarnings()...)
	if report.Resources == nil && report.LockChanged == nil && len(report.Warnings) == 0 {
		return nil
	}
	if o.asJSON {
		return writeJSON(out, report)
	}
	_, err := io.WriteString(out, reportEnd(newTextStyle(out), method, report, o.streamed))
	return err
}

func (o *commandOutput) reconciled(out io.Writer, report reconcile.Report) error {
	if err := o.stop(); err != nil {
		return err
	}
	report.Warnings = append(report.Warnings, o.takeWarnings()...)
	if report.Head == "" && len(report.Warnings) == 0 {
		return nil
	}
	if o.asJSON {
		return writeJSON(out, report)
	}
	if report.Update == nil {
		return nil
	}
	_, err := io.WriteString(out, proposalsEnd(newTextStyle(out), report.Update))
	return err
}
