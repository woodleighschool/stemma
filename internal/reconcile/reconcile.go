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
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/reconcile/git"
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
// reconciled, and the cache and state directories are the ones every
// command uses.
type Options struct {
	ConfigPath         string
	CacheDir, StateDir string
	// ResourceDone streams engine results for progress display.
	ResourceDone func(method string, resource engine.ResourceReport) error
}

// Report summarises one run.
type Report struct {
	Branch  string   `json:"branch"`
	Head    string   `json:"head"`
	Apply   *Apply   `json:"apply,omitempty"`
	Updates []Update `json:"updates,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Apply reports the reviewed branch's publication.
type Apply struct {
	Commit  string `json:"commit"`
	Skipped bool   `json:"skipped"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Update reports one proposal branch.
type Update struct {
	Resource    string `json:"resource"`
	Branch      string `json:"branch,omitempty"`
	Action      string `json:"action"`
	PullRequest string `json:"pull_request,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Error       string `json:"error,omitempty"`
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
// request per resource whose current inputs differ from the lock. The phases
// fail independently: a broken apply never blocks update maintenance.
func Run(ctx context.Context, opts Options) (report Report, runErr error) {
	defer func() {
		if runErr != nil {
			report.Error = runErr.Error()
		}
	}()
	r, err := newRunner(ctx, opts)
	if err != nil {
		return report, err
	}
	done := plugin.Stage(ctx, "Fetching origin")
	head, err := r.sync(ctx)
	done(err)
	if err != nil {
		return report, err
	}
	report.Branch, report.Head = r.base, head
	reviewed, err := r.repo.Worktree(head)
	if err != nil {
		return report, err
	}
	defer func() { _ = reviewed.Remove() }()
	// The report carries each failure; the error names the phases that failed.
	var failures []string
	apply, err := r.apply(ctx, head, reviewed)
	report.Apply = &apply
	if err != nil {
		failures = append(failures, fmt.Sprintf("apply %s: %s", short(head), firstLine(err.Error())))
	}
	report.Updates, err = r.update(ctx, head, reviewed)
	if err != nil {
		failed := 0
		for _, update := range report.Updates {
			if update.Error != "" {
				failed++
			}
		}
		if failed > 0 {
			failures = append(failures, "update: "+plural(failed, "resource")+" failed")
		} else {
			failures = append(failures, "update: "+firstLine(err.Error()))
		}
	}
	if len(failures) > 0 {
		return report, errors.New(strings.Join(failures, "; "))
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
	switch spec.Type {
	case "github":
		return github.New(remote, spec.Config)
	default:
		return nil, fmt.Errorf("reconcile: unsupported source_control type %q", spec.Type)
	}
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
	opts := engine.Options{ConfigPath: configPath, CacheDir: r.opts.CacheDir, StateDir: r.stateDir, Method: method, Resources: resources, Lock: lockfile.Options{Frozen: true, Offline: offline}}
	if r.opts.ResourceDone != nil {
		opts.ResourceDone = func(resource engine.ResourceReport) error { return r.opts.ResourceDone(method, resource) }
	}
	return opts
}

// apply publishes the reviewed commit offline once. The marker moves only
// after every destination succeeded, so a partial apply is retried next run.
func (r *runner) apply(ctx context.Context, head string, reviewed *git.Worktree) (Apply, error) {
	result := Apply{Commit: head}
	m, err := readMarker(r.stateDir)
	if err != nil {
		return result, err
	}
	if m.Applied == head {
		result.Skipped = true
		return result, nil
	}
	done := plugin.Stage(ctx, "Applying reviewed branch")
	report, applyErr := engine.Run(ctx, r.engineOptions("apply", r.configIn(reviewed), nil, true))
	done(applyErr)
	state, summary := applySummary(report, applyErr)
	result.Summary = summary
	if applyErr != nil {
		result.Error = applyErr.Error()
	}
	statusErr := r.host.SetCommitStatus(ctx, head, sourcecontrol.Status{Name: applyContext, State: state, Description: summary})
	if applyErr == nil {
		if err := writeMarker(r.stateDir, head); err != nil {
			return result, errors.Join(statusErr, err)
		}
	}
	return result, errors.Join(applyErr, statusErr)
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
// on its own branch and retires branches that no longer propose anything.
func (r *runner) update(ctx context.Context, head string, reviewed *git.Worktree) ([]Update, error) {
	branches, err := r.repo.Branches(prefix)
	if err != nil {
		return nil, err
	}
	hints, err := r.hints(reviewed, branches)
	if err != nil {
		return nil, err
	}
	done := plugin.Stage(ctx, "Resolving catalog")
	candidate, err := engine.Resolve(ctx, engine.Options{ConfigPath: r.configIn(reviewed), CacheDir: r.opts.CacheDir, Lock: lockfile.Options{Hints: hints}})
	done(err)
	if err != nil {
		return nil, err
	}
	open, err := r.host.PullRequests(ctx, sourcecontrol.Open, "")
	if err != nil {
		return nil, err
	}
	pulls := map[string]sourcecontrol.PullRequest{}
	for _, pull := range open {
		pulls[pull.Head] = pull
	}
	var updates []Update
	var failures []error
	keep := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(candidate.Resources)) {
		if resource := candidate.Resources[key]; resource.Error != "" {
			// An unresolved resource keeps whatever it proposed last time.
			keep[branchName(resource.Kind, resource.Name)] = true
			updates = append(updates, Update{Resource: resource.Kind + "/" + resource.Name, Action: "failed", Error: resource.Error})
			failures = append(failures, fmt.Errorf("%s: %s", key, resource.Error))
		}
	}
	changes := diff(candidate)
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		update, err := r.propose(ctx, head, key, changes[key], candidate, pulls)
		keep[update.Branch] = true
		if err != nil {
			update.Action = "failed"
			update.Error = err.Error()
			failures = append(failures, fmt.Errorf("%s: %w", key, err))
		}
		updates = append(updates, update)
	}
	for _, branch := range branches {
		if keep[branch] {
			continue
		}
		update, err := r.retire(ctx, branch, pulls)
		if err != nil {
			update.Action = "failed"
			update.Error = err.Error()
			failures = append(failures, fmt.Errorf("%s: %w", branch, err))
		}
		updates = append(updates, update)
	}
	return updates, errors.Join(failures...)
}

// hints collects the entries pending proposals recorded for resources the
// reviewed lock has not accepted, so refreshing them confirms the proposed
// bytes with a conditional request instead of downloading them every run.
func (r *runner) hints(reviewed *git.Worktree, branches []string) (map[string]map[string]source.Entry, error) {
	current, err := lockfile.Load(lockfile.Filename(filepath.Join(reviewed.Dir, r.project)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	hints := map[string]map[string]source.Entry{}
	for _, branch := range branches {
		managed, err := r.managed(branch)
		if err != nil {
			return nil, err
		}
		if !managed {
			continue
		}
		data, err := r.repo.Show("refs/remotes/origin/"+branch, r.lockPath)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			continue
		}
		proposed, err := lockfile.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", branch, err)
		}
		for key, entries := range proposed.Inputs {
			if !equalEntries(current.Inputs[key], entries) {
				hints[key] = entries
			}
		}
	}
	return hints, nil
}

// managed reports whether the reconciler still owns a branch: exactly one
// commit beyond the reviewed branch, carrying the trailer, authored and
// committed by the configured identity. A branch the reviewed branch already
// contains was merged and may be replaced or removed. Anything else was
// touched by a person and is left alone.
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
// verification did not succeed, and left alone once a person touched it.
func (r *runner) propose(ctx context.Context, head, key string, change change, candidate engine.Candidate, pulls map[string]sourcecontrol.PullRequest) (Update, error) {
	branch := branchName(change.kind, change.name)
	update := Update{Resource: change.kind + "/" + change.name, Branch: branch}
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("branch", branch))
	tip, err := r.repo.Tip("refs/remotes/origin/" + branch)
	if err != nil {
		return update, err
	}
	if tip != "" {
		managed, err := r.managed(branch)
		if err != nil {
			return update, err
		}
		if !managed {
			update.Action, update.Summary = "skipped", "branch has commits by a person"
			return update, nil
		}
	}
	file := candidate.Lock
	file.Inputs = maps.Clone(file.Inputs)
	if file.Inputs == nil {
		file.Inputs = map[string]map[string]source.Entry{}
	}
	if file.Version == 0 {
		file.Version = 2
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
			return update, err
		}
		if err := stabilise(file.Inputs[key], published, key); err != nil {
			return update, err
		}
	}
	data, err := encode(file)
	if err != nil {
		return update, err
	}
	pull, open := pulls[branch]
	if tip != "" {
		commit, err := r.repo.Commit(tip)
		if err != nil {
			return update, err
		}
		if !open {
			// A closed proposal stays declined while the resource would
			// propose the same entries, however the reviewed branch moved.
			declined, err := r.declined(ctx, branch, tip)
			if err != nil {
				return update, err
			}
			if declined && proposes(published, key, file.Inputs[key]) {
				update.Action, update.Summary = "declined", "a person closed this proposal"
				return update, nil
			}
		}
		if commit.Parent == head && bytes.Equal(published, data) {
			state, err := r.host.CommitStatus(ctx, tip, planContext)
			if err != nil {
				return update, err
			}
			if state == sourcecontrol.Success && open {
				update.Action, update.PullRequest, update.Summary = "unchanged", pull.URL, pull.Title
				return update, nil
			}
			update.Action = "retried"
			worktree, err := r.repo.Worktree(tip)
			if err != nil {
				return update, err
			}
			defer func() { _ = worktree.Remove() }()
			return r.publish(ctx, update, key, change, candidate, worktree, tip, "", pull, open)
		}
	}
	done := plugin.Stage(ctx, "Proposing lock change")
	worktree, err := r.repo.Worktree(head)
	if err != nil {
		done(err)
		return update, err
	}
	defer func() { _ = worktree.Remove() }()
	if err := lockfile.Save(filepath.Join(worktree.Dir, r.project), file); err != nil {
		done(err)
		return update, err
	}
	commit, err := worktree.Commit(commitMessage(change)+"\n\n"+trailer, r.lockPath)
	done(err)
	if err != nil {
		return update, err
	}
	update.Action = "created"
	if tip != "" {
		update.Action = "updated"
	}
	return r.publish(ctx, update, key, change, candidate, worktree, commit, tip, pull, open)
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

// stabilise keeps the branch's recorded timestamp for inputs whose bytes the
// candidate resolved unchanged, so re-resolving never rewrites a proposal.
func stabilise(entries map[string]source.Entry, published []byte, key string) error {
	if len(published) == 0 || len(entries) == 0 {
		return nil
	}
	file, err := lockfile.Parse(published)
	if err != nil {
		return fmt.Errorf("proposed lockfile: %w", err)
	}
	for name, entry := range entries {
		if previous, ok := file.Inputs[key][name]; ok && previous.Content.Artifact == entry.Content.Artifact && !previous.ResolvedAt.IsZero() {
			entry.ResolvedAt = previous.ResolvedAt
			entries[name] = entry
		}
	}
	return nil
}

// publish verifies the proposal commit the worktree has checked out, pushes it
// when it is new, keeps its pull request current and records the verification
// as a commit status.
func (r *runner) publish(ctx context.Context, update Update, key string, change change, candidate engine.Candidate, worktree *git.Worktree, commit, expected string, pull sourcecontrol.PullRequest, open bool) (Update, error) {
	result := r.verify(ctx, worktree, key, change, candidate)
	if update.Action != "retried" {
		done := plugin.Stage(ctx, "Pushing proposal")
		err := worktree.Push(ctx, update.Branch, expected)
		done(err)
		if err != nil {
			return update, err
		}
	}
	description := body(change, candidate.Lock.Inputs[key], change.entries, result)
	if open {
		if err := r.host.UpdatePullRequest(ctx, pull.Number, title(change, result), description); err != nil {
			return update, err
		}
	} else {
		var err error
		pull, err = r.host.CreatePullRequest(ctx, update.Branch, r.base, title(change, result), description)
		if err != nil {
			return update, err
		}
	}
	update.PullRequest = pull.URL
	update.Summary = planSummary(change, result)
	if err := r.host.SetCommitStatus(ctx, commit, sourcecontrol.Status{Name: planContext, State: result.state, Description: update.Summary}); err != nil {
		return update, err
	}
	return update, result.err
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
		_, result.err = engine.ValidateProject(ctx, engine.Options{ConfigPath: configPath, CacheDir: r.opts.CacheDir, Lock: lockfile.Options{Offline: true}})
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
		for _, resource := range result.planned.Resources {
			if resource.Key == key {
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
// from the reviewed lock. Branches a person touched are left alone.
func (r *runner) retire(ctx context.Context, branch string, pulls map[string]sourcecontrol.PullRequest) (Update, error) {
	update := Update{Resource: strings.TrimPrefix(branch, prefix), Branch: branch}
	managed, err := r.managed(branch)
	if err != nil {
		return update, err
	}
	if !managed {
		update.Action, update.Summary = "skipped", "branch has commits by a person"
		return update, nil
	}
	tip, err := r.repo.Tip("refs/remotes/origin/" + branch)
	if err != nil {
		return update, err
	}
	merged, err := r.merged(ctx, branch, tip)
	if err != nil {
		return update, err
	}
	if pull, open := pulls[branch]; open {
		if err := r.host.ClosePullRequest(ctx, pull.Number); err != nil {
			return update, err
		}
		update.PullRequest = pull.URL
	}
	if err := r.repo.DeleteBranch(ctx, branch, tip); err != nil {
		return update, err
	}
	update.Action, update.Summary = "retired", "reviewed lock no longer differs"
	if merged {
		update.Summary = "merged into the reviewed branch"
	}
	return update, nil
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
