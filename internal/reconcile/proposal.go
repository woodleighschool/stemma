package reconcile

import (
	"bytes"
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/git"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

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
	done := plugin.Stage(ctx, "Resolving catalog")
	candidate, err := engine.Resolve(ctx, r.engineOptions("update", r.configIn(reviewed), nil))
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
	if !change.removed {
		proposal.Plan = &result.plan
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
// planned against the exact proposed lock, acquiring its recorded inputs as needed.
func (r *runner) verify(ctx context.Context, worktree *git.Worktree, key string, change change, candidate engine.Candidate) verification {
	var result verification
	configPath := r.configIn(worktree)
	if change.removed {
		done := plugin.Stage(ctx, "Validating catalog")
		_, result.err = engine.ValidateProject(ctx, engine.Options{ConfigPath: configPath, CacheDir: r.opts.CacheDir}, true)
		done(result.err)
	} else {
		closure := append([]string{key}, candidate.Dependents(key)...)
		done := plugin.Stage(ctx, "Planning proposal")
		result.plan, result.err = engine.Run(ctx, r.engineOptions("plan", configPath, closure))
		done(result.err)
		for _, resource := range result.plan.Resources {
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
