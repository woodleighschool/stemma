// Package reconcile publishes the reviewed branch of the repository holding a
// project and proposes lock updates as pull requests, in one finite run.
package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/git"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol/github"
	"github.com/woodleighschool/stemma/internal/source"
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

// Proposal reports one proposal branch.
type Proposal struct {
	Resource    string         `json:"resource"`
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

// Run applies the reviewed branch when it changed, then proposes one pull
// request per resource whose current inputs differ from the lock. Both phases
// read the reviewed project, so when it does not load neither runs. Otherwise
// they fail independently: a broken apply never blocks update maintenance.
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
	host, err := openSourceControl(document.Spec.Reconcile.SourceControl, repo.Remote)
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

func openSourceControl(spec config.SourceControl, remote string) (sourcecontrol.Provider, error) {
	if spec.Type != "github" {
		return nil, fmt.Errorf("reconcile: unsupported source_control type %q", spec.Type)
	}
	settings, err := spec.ResolvedConfig()
	if err != nil {
		return nil, fmt.Errorf("reconcile: source_control config: %w", err)
	}
	return github.New(remote, settings)
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

func (r *runner) engineOptions(method, configPath string, resources []string, offline bool) engine.Options {
	opts := engine.Options{ConfigPath: configPath, CacheDir: r.opts.CacheDir, Method: method, Resources: resources, Lock: lockfile.Options{Offline: offline}}
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

// apply publishes the reviewed commit offline once. The marker moves only
// after every destination succeeded, so a partial apply is retried next run.
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
	report, applyErr := engine.Run(ctx, r.engineOptions("apply", r.configIn(reviewed), nil, true))
	result.Report = &report
	done(applyErr)
	state, summary := applySummary(report, applyErr)
	result.Summary = summary
	if summary == "" {
		summary = failedDescription
	}
	failures := []error{engine.Unreported(applyErr)}
	if err := r.host.SetCommitStatus(ctx, head, sourcecontrol.Status{Name: applyContext, State: state, Description: summary}); err != nil {
		failures = append(failures, fmt.Errorf("commit status: %w", err))
	}
	if applyErr == nil {
		failures = append(failures, writeMarker(r.stateDir, head))
	}
	if err := errors.Join(failures...); err != nil {
		result.Error = err.Error()
	}
	return result
}

// change is one resource whose lock differs from its current inputs.
type change struct {
	kind, name string
	entries    map[string]source.Entry
	removed    bool
	// refresh marks a change that resolved the same bytes with new metadata.
	refresh bool
}

// diff finds resources whose resolved inputs differ from the reviewed lock and
// locked resources the catalog no longer declares. A suspended resource keeps
// whatever the lock holds for it.
func diff(candidate engine.Candidate) map[string]change {
	changes := map[string]change{}
	for key, resource := range candidate.Resources {
		if resource.Suspended || resource.Error != "" || equalEntries(candidate.Lock.Inputs[key], resource.Inputs) {
			continue
		}
		changes[key] = change{kind: resource.Kind, name: resource.Name, entries: resource.Inputs, refresh: sameArtifacts(candidate.Lock.Inputs[key], resource.Inputs)}
	}
	for key := range candidate.Lock.Inputs {
		if _, declared := candidate.Resources[key]; !declared {
			kind, name := splitKey(key)
			changes[key] = change{kind: kind, name: name, removed: true}
		}
	}
	return changes
}

// sameArtifacts reports whether every input resolved to the bytes already
// locked, so only observations, filenames or declarations changed.
func sameArtifacts(before, after map[string]source.Entry) bool {
	if len(before) != len(after) {
		return false
	}
	for name, entry := range after {
		previous, ok := before[name]
		if !ok || previous.Content.Artifact != entry.Content.Artifact {
			return false
		}
	}
	return true
}

func equalEntries(before, after map[string]source.Entry) bool {
	if len(before) == 0 && len(after) == 0 {
		return true
	}
	left, _ := json.Marshal(before)
	right, _ := json.Marshal(after)
	return bytes.Equal(left, right)
}

func splitKey(key string) (kind, name string) {
	parts := strings.Split(key, "/")
	if len(parts) < 2 {
		return key, key
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

var (
	unsafeRef = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	dotRuns   = regexp.MustCompile(`\.{2,}`)
)

// branchName derives a valid branch name from a resource identity.
func branchName(kind, name string) string {
	for strings.HasSuffix(name, ".lock") {
		name = strings.TrimSuffix(name, ".lock")
	}
	name = strings.Trim(dotRuns.ReplaceAllString(unsafeRef.ReplaceAllString(name, "-"), "."), "-.")
	if name == "" {
		name = "resource"
	}
	return prefix + kind + "/" + name
}

// update resolves the reviewed catalog once, proposes each differing resource
// on its own branch and retires branches that no longer propose anything. The
// result holds every failure; the error is an interruption or a proposal the
// caller could not take.
func (r *runner) update(ctx context.Context, head string, reviewed *git.Worktree) (Update, error) {
	var result Update
	// stop records the failure that ended the phase before any proposal.
	stop := func(err error) (Update, error) {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.Error = err.Error()
		return result, nil
	}
	branches, err := r.repo.Branches(prefix)
	if err != nil {
		return stop(err)
	}
	opts := engine.Options{ConfigPath: r.configIn(reviewed), CacheDir: r.opts.CacheDir}
	if r.opts.ResourceDone != nil {
		opts.ResourceDone = func(resource engine.ResourceReport) error { return r.opts.ResourceDone("update", resource) }
	}
	done := plugin.Stage(ctx, "Resolving catalog")
	candidate, err := engine.Resolve(ctx, opts)
	done(err)
	if err != nil {
		return stop(err)
	}
	open, err := r.host.PullRequests(ctx, sourcecontrol.Open, "")
	if err != nil {
		return stop(err)
	}
	pulls := map[string]sourcecontrol.PullRequest{}
	for _, pull := range open {
		pulls[pull.Head] = pull
	}
	record := func(proposal Proposal) error {
		result.Proposals = append(result.Proposals, proposal)
		if r.opts.ProposalDone != nil {
			return r.opts.ProposalDone(proposal)
		}
		return nil
	}
	keep := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(candidate.Resources)) {
		if resource := candidate.Resources[key]; resource.Error != "" {
			// An unresolved resource keeps whatever it proposed last time.
			keep[branchName(resource.Kind, resource.Name)] = true
			action := "failed"
			if len(resource.BlockedBy) > 0 {
				action = "blocked"
			}
			if err := record(Proposal{Resource: resource.Kind + "/" + resource.Name, Action: action, Error: resource.Error}); err != nil {
				return result, err
			}
		}
	}
	changes := diff(candidate)
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		proposal, err := r.propose(ctx, head, key, changes[key], candidate, pulls)
		keep[proposal.Branch] = true
		if err != nil {
			proposal.Action, proposal.Error = "failed", err.Error()
		}
		if err := record(proposal); err != nil {
			return result, err
		}
	}
	for _, branch := range branches {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if keep[branch] {
			continue
		}
		proposal, err := r.retire(ctx, branch, pulls)
		if err != nil {
			proposal.Action, proposal.Error = "failed", err.Error()
		}
		if err := record(proposal); err != nil {
			return result, err
		}
	}
	return result, nil
}

// managed reports whether the reconciler still owns a branch: exactly one
// commit beyond the reviewed branch, carrying the trailer, with the configured
// identity as its author and committer. A branch the reviewed branch already
// contains was merged and may be replaced or removed. Anything else was
// touched by a person and stays as it is.
func (r *runner) managed(branch string) (bool, error) {
	ref := "refs/remotes/origin/" + branch
	count, err := r.repo.Count("refs/remotes/origin/"+r.base, ref)
	if err != nil {
		return false, err
	}
	if count == 0 {
		return true, nil
	}
	if count != 1 {
		return false, nil
	}
	commit, err := r.repo.Commit(ref)
	if err != nil {
		return false, err
	}
	key, value, _ := strings.Cut(trailer, ": ")
	email := r.repo.Identity.Email
	return commit.AuthorEmail == email && commit.CommitterEmail == email && commit.Trailers[key] == value, nil
}

// propose keeps one branch per resource: regenerated from the reviewed branch
// whenever the proposal or its base moved, verified again when its last
// verification did not succeed, and kept as it is once a person touched it.
func (r *runner) propose(ctx context.Context, head, key string, change change, candidate engine.Candidate, pulls map[string]sourcecontrol.PullRequest) (Proposal, error) {
	branch := branchName(change.kind, change.name)
	proposal := Proposal{Resource: change.kind + "/" + change.name, Branch: branch}
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("branch", branch))
	tip, err := r.repo.Tip("refs/remotes/origin/" + branch)
	if err != nil {
		return proposal, err
	}
	if tip != "" {
		managed, err := r.managed(branch)
		if err != nil {
			return proposal, err
		}
		if !managed {
			proposal.Action, proposal.Summary = "skipped", "branch has commits by a person"
			return proposal, nil
		}
	}
	file := candidate.Lock
	file.Inputs = maps.Clone(file.Inputs)
	if file.Inputs == nil {
		file.Inputs = map[string]map[string]source.Entry{}
	}
	if file.Version == 0 {
		file.Version = lockfile.Version
		file.Plugins = candidate.Plugins
	}
	if len(change.entries) == 0 {
		delete(file.Inputs, key)
	} else {
		file.Inputs[key] = change.entries
	}
	var published []byte
	if tip != "" {
		published, err = r.repo.Show(tip, r.lockPath)
		if err != nil {
			return proposal, err
		}
	}
	data, err := encode(file)
	if err != nil {
		return proposal, err
	}
	pull, open := pulls[branch]
	if tip != "" {
		commit, err := r.repo.Commit(tip)
		if err != nil {
			return proposal, err
		}
		if !open {
			// A closed proposal stays declined while the resource would
			// propose the same entries, however the reviewed branch moved.
			declined, err := r.declined(ctx, branch, tip)
			if err != nil {
				return proposal, err
			}
			if declined && proposes(published, key, file.Inputs[key]) {
				proposal.Action, proposal.Summary = "declined", "a person closed this proposal"
				return proposal, nil
			}
		}
		if commit.Parent == head && bytes.Equal(published, data) {
			state, err := r.host.CommitStatus(ctx, tip, planContext)
			if err != nil {
				return proposal, err
			}
			if state == sourcecontrol.Success && open {
				proposal.Action, proposal.PullRequest, proposal.Summary = "unchanged", pull.URL, pull.Title
				return proposal, nil
			}
			proposal.Action = "retried"
			worktree, err := r.repo.Worktree(tip)
			if err != nil {
				return proposal, err
			}
			defer func() { _ = worktree.Remove() }()
			return r.publish(ctx, proposal, key, change, candidate, worktree, tip, "", pull, open)
		}
	}
	done := plugin.Stage(ctx, "Proposing lock change", plugin.Detail(proposal.Branch))
	worktree, err := r.repo.Worktree(head)
	if err != nil {
		done(err)
		return proposal, err
	}
	defer func() { _ = worktree.Remove() }()
	if err := lockfile.Save(filepath.Join(worktree.Dir, r.project), file); err != nil {
		done(err)
		return proposal, err
	}
	commit, err := worktree.Commit(commitMessage(change)+"\n\n"+trailer, r.lockPath)
	done(err)
	if err != nil {
		return proposal, err
	}
	proposal.Action = "created"
	if tip != "" {
		proposal.Action = "updated"
	}
	return r.publish(ctx, proposal, key, change, candidate, worktree, commit, tip, pull, open)
}

// closed returns the closed pull request that proposed this exact branch tip,
// if any. The host's record says whether a person merged or declined it.
func (r *runner) closed(ctx context.Context, branch, tip string) (*sourcecontrol.PullRequest, error) {
	pulls, err := r.host.PullRequests(ctx, sourcecontrol.Closed, branch)
	if err != nil {
		return nil, err
	}
	for i := range pulls {
		if pulls[i].HeadSHA == tip {
			return &pulls[i], nil
		}
	}
	return nil, nil
}

// declined reports whether a person closed the proposal at tip without merging it.
func (r *runner) declined(ctx context.Context, branch, tip string) (bool, error) {
	pull, err := r.closed(ctx, branch, tip)
	return pull != nil && !pull.Merged, err
}

// merged reports whether the reviewed branch accepted the proposal at tip:
// its history contains the commit, or the host merged the pull request by
// squashing or rebasing, which leaves the commit outside that history.
func (r *runner) merged(ctx context.Context, branch, tip string) (bool, error) {
	count, err := r.repo.Count("refs/remotes/origin/"+r.base, "refs/remotes/origin/"+branch)
	if err != nil {
		return false, err
	}
	if count == 0 {
		return true, nil
	}
	pull, err := r.closed(ctx, branch, tip)
	return pull != nil && pull.Merged, err
}

// proposes reports whether a published proposal already records these
// entries for the resource; an absent lockfile proposes nothing.
func proposes(published []byte, key string, entries map[string]source.Entry) bool {
	if len(published) == 0 {
		return len(entries) == 0
	}
	file, err := lockfile.Parse(published)
	if err != nil {
		return false
	}
	return equalEntries(file.Inputs[key], entries)
}

// encode renders the bytes Save would leave in the worktree: nothing at all
// once no input or plugin remains locked.
func encode(file lockfile.File) ([]byte, error) {
	if len(file.Inputs) == 0 && len(file.Plugins) == 0 {
		return nil, nil
	}
	return lockfile.Encode(file)
}

// publish verifies the proposal commit the worktree has checked out, pushes it
// when it is new, keeps its pull request current and records the verification
// as a commit status.
func (r *runner) publish(ctx context.Context, proposal Proposal, key string, change change, candidate engine.Candidate, worktree *git.Worktree, commit, expected string, pull sourcecontrol.PullRequest, open bool) (Proposal, error) {
	result := r.verify(ctx, worktree, key, change, candidate)
	if result.planned.Resources != nil {
		proposal.Plan = &result.planned
	} else if result.prepared.Resources != nil {
		proposal.Plan = &result.prepared
	}
	if proposal.Action != "retried" {
		done := plugin.Stage(ctx, "Pushing proposal", plugin.Detail(proposal.Branch))
		err := worktree.Push(ctx, proposal.Branch, expected)
		done(err)
		if err != nil {
			return proposal, err
		}
	}
	description := body(change, candidate.Lock.Inputs[key], change.entries, result)
	if open {
		if err := r.host.UpdatePullRequest(ctx, pull.Number, title(change, result), description); err != nil {
			return proposal, err
		}
	} else {
		var err error
		pull, err = r.host.CreatePullRequest(ctx, proposal.Branch, r.base, title(change, result), description)
		if err != nil {
			return proposal, err
		}
	}
	proposal.PullRequest = pull.URL
	proposal.Summary = planSummary(change, result)
	if err := r.host.SetCommitStatus(ctx, commit, sourcecontrol.Status{Name: planContext, State: result.state, Description: proposal.Summary}); err != nil {
		return proposal, err
	}
	return proposal, result.err
}

// verify proves a proposal from its worktree: a removed resource must still
// validate; a changed resource and everything consuming its outputs are
// prepared online from the exact locked observations, then planned offline,
// which shows every byte a merge will need is already cached.
func (r *runner) verify(ctx context.Context, worktree *git.Worktree, key string, change change, candidate engine.Candidate) verification {
	var result verification
	configPath := r.configIn(worktree)
	if change.removed {
		done := plugin.Stage(ctx, "Validating catalog")
		_, result.err = engine.ValidateProject(ctx, engine.Options{ConfigPath: configPath, CacheDir: r.opts.CacheDir, Lock: lockfile.Options{Offline: true}}, true)
		done(result.err)
	} else {
		closure := append([]string{key}, candidate.Dependents(key)...)
		done := plugin.Stage(ctx, "Preparing proposal")
		result.prepared, result.err = engine.Run(ctx, r.engineOptions("prepare", configPath, closure, false))
		done(result.err)
		if result.err == nil {
			done = plugin.Stage(ctx, "Planning proposal offline")
			result.planned, result.err = engine.Run(ctx, r.engineOptions("plan", configPath, closure, true))
			done(result.err)
		}
		for _, resource := range slices.Concat(result.prepared.Resources, result.planned.Resources) {
			if resource.Key == key && resource.Artifacts["installer"].Version != "" {
				result.version = resource.Artifacts["installer"].Version
			}
		}
	}
	result.state = sourcecontrol.Success
	if result.err != nil {
		result.state = sourcecontrol.Failure
	}
	result.summary = planSummary(change, result)
	return result
}

// retire closes and deletes a managed branch whose resource no longer differs
// from the reviewed lock. Branches a person touched stay as they are.
func (r *runner) retire(ctx context.Context, branch string, pulls map[string]sourcecontrol.PullRequest) (Proposal, error) {
	proposal := Proposal{Resource: strings.TrimPrefix(branch, prefix), Branch: branch}
	managed, err := r.managed(branch)
	if err != nil {
		return proposal, err
	}
	if !managed {
		proposal.Action, proposal.Summary = "skipped", "branch has commits by a person"
		return proposal, nil
	}
	tip, err := r.repo.Tip("refs/remotes/origin/" + branch)
	if err != nil {
		return proposal, err
	}
	merged, err := r.merged(ctx, branch, tip)
	if err != nil {
		return proposal, err
	}
	if pull, open := pulls[branch]; open {
		if err := r.host.ClosePullRequest(ctx, pull.Number); err != nil {
			return proposal, err
		}
		proposal.PullRequest = pull.URL
	}
	if err := r.repo.DeleteBranch(ctx, branch, tip); err != nil {
		return proposal, err
	}
	proposal.Action, proposal.Summary = "retired", "reviewed lock no longer differs"
	if merged {
		proposal.Summary = "merged into the reviewed branch"
	}
	return proposal, nil
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
