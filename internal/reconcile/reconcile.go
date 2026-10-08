// Package reconcile publishes the reviewed branch of the repository holding a
// project and proposes lock updates as pull requests, in one finite run.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/git"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol/github"
	"github.com/woodleighschool/stemma/plugin"
)

const (
	// trailer marks commits the reconciler generated; a branch stays managed
	// only while its single commit carries it.
	trailer = "Stemma-Managed: reconcile/v1"
	// prefix namespaces the branches the reconciler owns.
	prefix = "stemma/"
)

// Options locate the project. The checkout holding it is the repository
// reconciled; the cache directory is the one every command uses, and the state
// directory holds the applied marker.
type Options struct {
	ConfigPath         string
	CacheDir, StateDir string
	// PhaseStarted announces apply or update before its work begins.
	PhaseStarted func(method string) error
	// ResourceDone streams each engine run's results with the run's method;
	// the catalog lookup reports as update.
	ResourceDone func(method string, resource engine.ResourceReport) error
	// ApplyDone receives the report once the reviewed branch's publication
	// finished, before any proposal.
	ApplyDone func(Report) error
	// ProposalDone streams each proposal branch's outcome.
	ProposalDone func(Proposal) error
}

// ErrFailed reports a run in which a phase or proposal failed. The report
// carries each failure where it happened.
var ErrFailed = errors.New("reconcile: a phase failed")

// Report summarises one run. A phase that did not run is absent; Error is the
// failure that stopped the run before or between them.
type Report struct {
	Branch   string   `json:"branch"`
	Head     string   `json:"head"`
	Apply    *Apply   `json:"apply,omitempty"`
	Update   *Update  `json:"update,omitempty"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Apply reports the reviewed branch's publication. Resource failures stay with
// their resources in Report; Error is the rest: what stopped the run, or a
// status or marker that could not be recorded.
type Apply struct {
	Commit  string         `json:"commit"`
	Skipped bool           `json:"skipped"`
	Summary string         `json:"summary,omitempty"`
	Error   string         `json:"error,omitempty"`
	Report  *engine.Report `json:"report,omitempty"`
}

// Failed reports whether the publication or its record failed.
func (a Apply) Failed() bool { return a.Error != "" || a.Report != nil && a.Report.Error != "" }

// Update reports the update phase: the failure that stopped it before any
// proposal, or each proposal branch's outcome.
type Update struct {
	Error     string     `json:"error,omitempty"`
	Proposals []Proposal `json:"proposals,omitempty"`
}

// Failed reports whether the phase or any proposal failed. A blocked proposal
// waits on a failure reported for its producer.
func (u Update) Failed() bool {
	return u.Error != "" || slices.ContainsFunc(u.Proposals, func(proposal Proposal) bool { return proposal.Action == "failed" })
}

// Proposal reports one proposal branch, or a resource whose inputs could not
// be resolved.
type Proposal struct {
	// Name is the resource an update proposes, or names the lock refresh.
	Name        string         `json:"name"`
	Branch      string         `json:"branch,omitempty"`
	Action      string         `json:"action"`
	PullRequest string         `json:"pull_request,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	Error       string         `json:"error,omitempty"`
	Plan        *engine.Report `json:"plan,omitempty"`
}

type runner struct {
	opts Options
	repo *git.Repository
	host sourcecontrol.Provider
	// base is the reviewed branch: the one origin's HEAD points at.
	base string
	// project is the project directory relative to the checkout and config
	// the Project file's name, so the same project is found in any worktree.
	project, config string
	// lockPath is the lockfile's repository path.
	lockPath string
	stateDir string
}

// Run applies the reviewed branch when it changed, then proposes what differs
// from the lock: one pull request per resource whose content changed and one
// lock refresh for every difference that leaves content as reviewed. Both
// phases read the reviewed project, so when it does not load neither runs.
// Otherwise they fail independently: a broken apply never blocks update
// maintenance.
func Run(ctx context.Context, opts Options) (report Report, runErr error) {
	defer func() {
		if runErr != nil && runErr != ErrFailed { //nolint:errorlint // The sentinel alone means the report holds every failure.
			report.Error = runErr.Error()
		}
	}()
	r, err := newRunner(ctx, opts)
	if err != nil {
		return report, err
	}
	done := plugin.Stage(ctx, "Fetching origin")
	head, err := r.sync(ctx)
	done(err, plugin.Detail(r.base))
	if err != nil {
		return report, err
	}
	report.Branch, report.Head = r.base, head
	reviewed, err := r.repo.Worktree(head)
	if err != nil {
		return report, err
	}
	defer func() { _ = reviewed.Remove() }()
	if err := r.load(reviewed); err != nil {
		err = fmt.Errorf("%s@%s: %w", r.base, short(head), err)
		return report, errors.Join(err, r.reject(ctx, head))
	}
	if opts.PhaseStarted != nil {
		if err := opts.PhaseStarted("apply"); err != nil {
			return report, err
		}
	}
	apply := r.apply(ctx, head, reviewed)
	report.Apply = &apply
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if opts.ApplyDone != nil {
		if err := opts.ApplyDone(report); err != nil {
			return report, err
		}
	}
	if opts.PhaseStarted != nil {
		if err := opts.PhaseStarted("update"); err != nil {
			return report, err
		}
	}
	update, err := r.update(ctx, head, reviewed)
	report.Update = &update
	if err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if apply.Failed() || update.Failed() {
		return report, ErrFailed
	}
	return report, nil
}

// newRunner reads the source-control settings from the selected project and
// binds its checkout to the provider.
func newRunner(ctx context.Context, opts Options) (*runner, error) {
	configPath, err := filepath.Abs(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	document, err := config.LoadDocument(configPath)
	if err != nil {
		return nil, err
	}
	if document.Spec.Reconcile == nil {
		return nil, errors.New("reconcile: the Project declares no reconcile.source_control")
	}
	root := filepath.Dir(configPath)
	repo, err := git.Open(root)
	if err != nil {
		return nil, err
	}
	if repo.Shallow {
		return nil, errors.New("reconcile: the checkout is shallow; clone it with history so proposals can be told apart from reviewed commits")
	}
	if repo.Remote == "" {
		return nil, errors.New("reconcile: the checkout has no origin URL")
	}
	host, err := openSourceControl(ctx, document.Spec.Reconcile.SourceControl, repo.Remote)
	if err != nil {
		return nil, err
	}
	repo.Credentials = host.Credentials
	identity, err := host.Identity(ctx)
	if err != nil {
		return nil, err
	}
	repo.Identity = git.Identity(identity)
	project, err := filepath.Rel(repo.Dir, root)
	if err != nil {
		return nil, err
	}
	r := &runner{opts: opts, repo: repo, host: host, project: project, config: filepath.Base(configPath), stateDir: opts.StateDir}
	r.lockPath = path.Join(filepath.ToSlash(project), filepath.Base(lockfile.Filename("")))
	if r.stateDir == "" {
		r.stateDir = filepath.Join(root, ".stemma", "state")
	}
	return r, nil
}

func openSourceControl(ctx context.Context, spec config.SourceControl, remote string) (sourcecontrol.Provider, error) {
	if spec.Type != "github" {
		return nil, fmt.Errorf("reconcile: unsupported source_control type %q", spec.Type)
	}
	settings, err := spec.ResolvedConfig()
	if err != nil {
		return nil, fmt.Errorf("reconcile: source_control config: %w", err)
	}
	return github.New(ctx, remote, settings)
}

// sync fetches the reviewed branch and every managed branch from origin and
// returns the reviewed tip.
func (r *runner) sync(ctx context.Context) (string, error) {
	base, err := r.repo.DefaultBranch(ctx)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(base, prefix) {
		return "", fmt.Errorf("reconcile: the default branch %s lies inside the managed branch namespace", base)
	}
	r.base = base
	if err := r.repo.Fetch(ctx, "+refs/heads/"+base+":refs/remotes/origin/"+base, "+refs/heads/"+prefix+"*:refs/remotes/origin/"+prefix+"*"); err != nil {
		return "", err
	}
	head, err := r.repo.Tip("refs/remotes/origin/" + base)
	if err != nil {
		return "", err
	}
	if head == "" {
		return "", fmt.Errorf("reconcile: origin has no branch %s", base)
	}
	return head, nil
}

// configIn locates the Project file inside a worktree.
func (r *runner) configIn(worktree *git.Worktree) string {
	return filepath.Join(worktree.Dir, r.project, r.config)
}

func (r *runner) engineOptions(method, configPath string, resources []string) engine.Options {
	opts := engine.Options{ConfigPath: configPath, CacheDir: r.opts.CacheDir, Method: method, Resources: resources}
	if r.opts.ResourceDone != nil {
		opts.ResourceDone = func(resource engine.ResourceReport) error { return r.opts.ResourceDone(method, resource) }
	}
	return opts
}

// load reads what both phases share: the reviewed project and its lockfile. A
// missing lockfile loads, since the update phase proposes one.
func (r *runner) load(reviewed *git.Worktree) error {
	configPath := r.configIn(reviewed)
	if _, err := config.Load(configPath); err != nil {
		return err
	}
	_, err := lockfile.Load(lockfile.Filename(filepath.Dir(configPath)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// reject records that a reviewed commit whose project does not load cannot
// apply, unless it applied before.
func (r *runner) reject(ctx context.Context, head string) error {
	m, err := readMarker(r.stateDir)
	if err != nil || m.Applied == head {
		return err
	}
	return r.host.SetCommitStatus(ctx, head, sourcecontrol.Status{Name: applyContext, State: sourcecontrol.Failure, Description: failedDescription})
}

// apply publishes the reviewed commit from its frozen lock. The marker moves after
// publication and its commit status succeed, so failures retry next run.
func (r *runner) apply(ctx context.Context, head string, reviewed *git.Worktree) Apply {
	result := Apply{Commit: head}
	m, err := readMarker(r.stateDir)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if m.Applied == head {
		result.Skipped = true
		return result
	}
	done := plugin.Stage(ctx, "Applying reviewed branch", plugin.Detail(short(head)))
	report, applyErr := engine.Run(ctx, r.engineOptions("apply", r.configIn(reviewed), nil))
	result.Report = &report
	done(applyErr)
	state, summary := applySummary(report, applyErr)
	result.Summary = summary
	if summary == "" {
		summary = failedDescription
	}
	failures := []error{engine.Unreported(applyErr)}
	statusErr := r.host.SetCommitStatus(ctx, head, sourcecontrol.Status{Name: applyContext, State: state, Description: summary})
	if statusErr != nil {
		failures = append(failures, fmt.Errorf("commit status: %w", statusErr))
	}
	if applyErr == nil && statusErr == nil {
		failures = append(failures, writeMarker(r.stateDir, head))
	}
	if err := errors.Join(failures...); err != nil {
		result.Error = err.Error()
	}
	return result
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
