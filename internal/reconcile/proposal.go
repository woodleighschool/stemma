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

// update resolves the reviewed catalog once, proposes each changeset on its
// own branch and retires branches that no longer propose anything. The result
// holds every failure; the error is an interruption or a proposal the caller
// could not take.
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
			// An unresolved resource may be what a branch proposes: its own
			// stays as it is and the lock refresh is not retired.
			keep[branchName(resource.Kind, resource.Name)], keep[refreshBranch] = true, true
			action := "failed"
			if len(resource.BlockedBy) > 0 {
				action = "blocked"
			}
			if err := record(Proposal{Name: resource.Kind + "/" + resource.Name, Action: action, Error: resource.Error}); err != nil {
				return result, err
			}
		}
	}
	for _, set := range changesets(candidate) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		proposal, err := r.propose(ctx, head, set, candidate, pulls)
		keep[set.branch] = true
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

// propose keeps one branch per changeset: regenerated from the reviewed branch
// whenever its lock or its base moved, planned again when its last plan did
// not succeed, and kept as it is once a person touched it.
func (r *runner) propose(ctx context.Context, head string, set changeset, candidate engine.Candidate, pulls map[string]sourcecontrol.PullRequest) (Proposal, error) {
	proposal := Proposal{Name: set.name(), Branch: set.branch}
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("branch", set.branch))
	tip, err := r.repo.Tip("refs/remotes/origin/" + set.branch)
	if err != nil {
		return proposal, err
	}
	if tip != "" {
		managed, err := r.managed(set.branch)
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
	for key, c := range set.changes {
		if len(c.after) == 0 {
			delete(file.Inputs, key)
		} else {
			file.Inputs[key] = c.after
		}
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
	pull, open := pulls[set.branch]
	if tip != "" {
		commit, err := r.repo.Commit(tip)
		if err != nil {
			return proposal, err
		}
		if !open {
			// A closed proposal stays declined while the set would propose the
			// same entries, however the reviewed branch moved.
			declined, err := r.declined(ctx, set.branch, tip)
			if err != nil {
				return proposal, err
			}
			if declined && proposes(published, set) {
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
			return r.publish(ctx, proposal, set, candidate, worktree, tip, "", pull, open)
		}
	}
	done := plugin.Stage(ctx, "Proposing lock change", plugin.Detail(set.branch))
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
	commit, err := worktree.Commit(commitMessage(set)+"\n\n"+trailer, r.lockPath)
	done(err)
	if err != nil {
		return proposal, err
	}
	proposal.Action = "created"
	if tip != "" {
		proposal.Action = "updated"
	}
	return r.publish(ctx, proposal, set, candidate, worktree, commit, tip, pull, open)
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

// publish plans the proposal commit the worktree has checked out, pushes it
// when it is new, keeps its pull request current and records the plan as a
// commit status.
func (r *runner) publish(ctx context.Context, proposal Proposal, set changeset, candidate engine.Candidate, worktree *git.Worktree, commit, expected string, pull sourcecontrol.PullRequest, open bool) (Proposal, error) {
	plan := r.plan(ctx, worktree, set, candidate)
	proposal.Plan = plan.report
	if proposal.Action != "retried" {
		done := plugin.Stage(ctx, "Pushing proposal", plugin.Detail(proposal.Branch))
		err := worktree.Push(ctx, proposal.Branch, expected)
		done(err)
		if err != nil {
			return proposal, err
		}
	}
	heading, description := title(set, plan), body(set, plan)
	if open {
		if err := r.host.UpdatePullRequest(ctx, pull.Number, heading, description); err != nil {
			return proposal, err
		}
	} else {
		var err error
		pull, err = r.host.CreatePullRequest(ctx, proposal.Branch, r.base, heading, description)
		if err != nil {
			return proposal, err
		}
	}
	proposal.PullRequest = pull.URL
	proposal.Summary = planSummary(set, plan)
	if err := r.host.SetCommitStatus(ctx, commit, sourcecontrol.Status{Name: planContext, State: plan.state(), Description: proposal.Summary}); err != nil {
		return proposal, err
	}
	return proposal, plan.err
}

// plan proves a proposal from its worktree: every resource whose entries
// change and everything consuming their outputs are planned against the exact
// proposed lock, acquiring its recorded inputs as needed. Entries that go with
// an undeclared resource leave nothing to plan.
func (r *runner) plan(ctx context.Context, worktree *git.Worktree, set changeset, candidate engine.Candidate) planned {
	var closure []string
	for key, c := range set.changes {
		if !c.removed {
			closure = append(append(closure, key), candidate.Dependents(key)...)
		}
	}
	if len(closure) == 0 {
		return planned{}
	}
	slices.Sort(closure)
	done := plugin.Stage(ctx, "Planning proposal")
	report, err := engine.Run(ctx, r.engineOptions("plan", r.configIn(worktree), slices.Compact(closure)))
	done(err)
	return planned{report: &report, err: err}
}

// retire closes and deletes a managed branch that no longer proposes anything.
// Branches a person touched stay as they are.
func (r *runner) retire(ctx context.Context, branch string, pulls map[string]sourcecontrol.PullRequest) (Proposal, error) {
	proposal := Proposal{Name: strings.TrimPrefix(branch, prefix), Branch: branch}
	if branch == refreshBranch {
		proposal.Name = refreshName
	}
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
	proposal.Action, proposal.Summary = "retired", "nothing left to propose"
	if merged {
		proposal.Summary = "merged into the reviewed branch"
	}
	return proposal, nil
}
