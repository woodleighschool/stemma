package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func human() *object.Signature {
	return &object.Signature{Name: "Human", Email: "human@example.com", When: time.Now()}
}

// seed creates a bare origin whose trunk holds one commit by a person.
func seed(t *testing.T) (bare, head string) {
	t.Helper()
	bare = filepath.Join(t.TempDir(), "catalog.git")
	origin, err := gogit.PlainInit(bare, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := origin.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/trunk")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "seed")
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/trunk")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("catalog\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	commit, err := tree.Commit("seed", &gogit.CommitOptions{Author: human(), Committer: human()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PushContext(t.Context(), &gogit.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{"refs/heads/trunk:refs/heads/trunk"}}); err != nil {
		t.Fatal(err)
	}
	return bare, commit.String()
}

func TestOpenBindsTheCheckoutToItsOrigin(t *testing.T) {
	bare, head := seed(t)

	// A clone records origin's HEAD; a bare init plus remote asks origin.
	clone := filepath.Join(t.TempDir(), "clone")
	cloned, err := gogit.PlainClone(clone, &gogit.CloneOptions{URL: bare})
	if err != nil {
		t.Fatal(err)
	}
	if err := cloned.Storer.SetReference(plumbing.NewSymbolicReference("refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")); err != nil {
		t.Fatal(err)
	}
	fetched := filepath.Join(t.TempDir(), "fetched")
	initialised, err := gogit.PlainInit(fetched, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initialised.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(clone, "nested"), fetched} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		repo, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if repo.Remote != bare || repo.Dir != filepath.Dir(dir) && repo.Dir != dir {
			t.Fatalf("remote %q dir %q", repo.Remote, repo.Dir)
		}
		branch, err := repo.DefaultBranch(t.Context())
		if err != nil || branch != "trunk" {
			t.Fatalf("default branch %q: %v", branch, err)
		}
	}

	// Proposals are commits on detached worktrees, pushed with a lease.
	repo, err := Open(clone)
	if err != nil {
		t.Fatal(err)
	}
	repo.Identity = Identity{Name: "stemma[bot]", Email: "1+stemma[bot]@users.noreply.github.com"}
	if tip, err := repo.Tip("refs/remotes/origin/trunk"); err != nil || tip != head {
		t.Fatalf("tip %q: %v", tip, err)
	}
	propose := func(content string) (*Worktree, string) {
		t.Helper()
		worktree, err := repo.Worktree(head)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = worktree.Remove() })
		if data, err := os.ReadFile(filepath.Join(worktree.Dir, "README.md")); err != nil || string(data) != "catalog\n" {
			t.Fatalf("worktree content %q: %v", data, err)
		}
		if err := os.WriteFile(filepath.Join(worktree.Dir, "stemma.lock.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		commit, err := worktree.Commit("propose\n\nStemma-Managed: reconcile/v1", "stemma.lock.yaml")
		if err != nil {
			t.Fatal(err)
		}
		return worktree, commit
	}
	first, commit := propose("version: 2\n")
	if err := first.Push(t.Context(), "stemma/test", ""); err != nil {
		t.Fatal(err)
	}
	second, replacement := propose("version: 2\ninputs: {}\n")
	if err := second.Push(t.Context(), "stemma/test", ""); err == nil {
		t.Fatal("push succeeded against a stale lease")
	}
	if err := second.Push(t.Context(), "stemma/test", commit); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fetch(t.Context(), "+refs/heads/trunk:refs/remotes/origin/trunk", "+refs/heads/stemma/*:refs/remotes/origin/stemma/*"); err != nil {
		t.Fatal(err)
	}
	described, err := repo.Commit("refs/remotes/origin/stemma/test")
	if err != nil || described.SHA != replacement || described.Parent != head || described.AuthorEmail != repo.Identity.Email || described.CommitterEmail != repo.Identity.Email || described.Trailers["Stemma-Managed"] != "reconcile/v1" {
		t.Fatalf("commit %+v: %v", described, err)
	}
	if data, err := repo.Show(replacement, "stemma.lock.yaml"); err != nil || string(data) != "version: 2\ninputs: {}\n" {
		t.Fatalf("show %q: %v", data, err)
	}
	if data, err := repo.Show(head, "stemma.lock.yaml"); err != nil || data != nil {
		t.Fatalf("missing path %q: %v", data, err)
	}
	if count, err := repo.Count(head, replacement); err != nil || count != 1 {
		t.Fatalf("count %d: %v", count, err)
	}
	if count, err := repo.Count(head, head); err != nil || count != 0 {
		t.Fatalf("count %d: %v", count, err)
	}
	if branches, err := repo.Branches("stemma/"); err != nil || len(branches) != 1 || branches[0] != "stemma/test" {
		t.Fatalf("branches %v: %v", branches, err)
	}
	if err := repo.DeleteBranch(t.Context(), "stemma/test", commit); err == nil {
		t.Fatal("delete succeeded against a stale lease")
	}
	if err := repo.DeleteBranch(t.Context(), "stemma/test", replacement); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fetch(t.Context(), "+refs/heads/trunk:refs/remotes/origin/trunk", "+refs/heads/stemma/*:refs/remotes/origin/stemma/*"); err != nil {
		t.Fatal(err)
	}
	if branches, err := repo.Branches("stemma/"); err != nil || len(branches) != 0 {
		t.Fatalf("branches %v after delete: %v", branches, err)
	}

	// The checkout itself was never touched.
	current, err := cloned.Head()
	if err != nil || current.Name() != "refs/heads/trunk" || current.Hash().String() != head {
		t.Fatalf("checkout HEAD moved to %v: %v", current, err)
	}
	tree, err := cloned.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if status, err := tree.Status(); err != nil || !status.IsClean() {
		t.Fatalf("checkout status: %v %s", err, status)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "worktrees")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worktrees were registered in the checkout")
	}

	if err := cloned.Storer.SetShallow([]plumbing.Hash{plumbing.NewHash(head)}); err != nil {
		t.Fatal(err)
	}
	if shallow, err := Open(clone); err != nil || !shallow.Shallow {
		t.Fatalf("shallow checkout not reported: %v", err)
	}
}

func TestMergeBaseFindsWhereTheBranchLeftRev(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/trunk")); err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(name string, minute int) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tree.Add(name); err != nil {
			t.Fatal(err)
		}
		signature := &object.Signature{Name: "Human", Email: "human@example.com", When: time.Date(2026, 9, 26, 12, minute, 0, 0, time.UTC)}
		hash, err := tree.Commit(name, &gogit.CommitOptions{Author: signature, Committer: signature})
		if err != nil {
			t.Fatal(err)
		}
		return hash.String()
	}
	checkout := func(branch string, create bool) {
		t.Helper()
		if err := tree.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Create: create}); err != nil {
			t.Fatal(err)
		}
	}
	forked := commit("base", 0)
	checkout("feature", true)
	commit("feature", 1)
	checkout("trunk", false)
	commit("later", 2)
	checkout("feature", false)

	r, err := Open(dir)
	if err != nil || r.Remote != "" {
		t.Fatalf("checkout without origin: %v %q", err, r.Remote)
	}
	// Commits trunk gained after the fork are not part of the comparison.
	for _, rev := range []string{"trunk", "refs/heads/trunk", forked} {
		if base, err := r.MergeBase(rev); err != nil || base != forked {
			t.Fatalf("merge base of %s: %s %v", rev, base, err)
		}
	}
	if _, err := r.MergeBase("missing"); err == nil {
		t.Fatal("an unknown revision had a merge base")
	}
}
