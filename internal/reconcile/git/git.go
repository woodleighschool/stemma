// Package git drives the checkout holding a project, and its origin.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-billy/v6/osfs"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
)

// Credentials supply the username and password presented to an HTTP origin.
type Credentials func(ctx context.Context) (username, password string, err error)

// Identity is the author and committer of every commit made here.
type Identity struct {
	Name, Email string
}

// Repository is the checkout whose origin remote is being reconciled.
type Repository struct {
	// Dir is the top-level directory of the checkout.
	Dir string
	// Remote is the origin URL as the checkout configures it.
	Remote string
	// Credentials authenticate fetches and pushes to an HTTP origin. An SSH
	// origin uses the SSH agent instead.
	Credentials Credentials
	// Identity signs the commits made in worktrees.
	Identity Identity

	repo   *gogit.Repository
	origin *gogit.Remote
}

// Commit describes one commit.
type Commit struct {
	SHA, Parent                 string
	AuthorEmail, CommitterEmail string
	Trailers                    map[string]string
}

const remoteName = "origin"

// Open locates the checkout containing dir and its origin remote.
func Open(dir string) (*Repository, error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, fmt.Errorf("git: %s: %w", dir, err)
	}
	shallow, err := repo.Storer.Shallow()
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	if len(shallow) > 0 {
		return nil, errors.New("git: the checkout is shallow; clone it with history so proposals can be told apart from reviewed commits")
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	origin, err := repo.Remote(remoteName)
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	if len(origin.Config().URLs) == 0 {
		return nil, errors.New("git: origin has no URL")
	}
	return &Repository{Dir: worktree.Filesystem().Root(), Remote: origin.Config().URLs[0], repo: repo, origin: origin}, nil
}

// options authenticate one exchange with origin.
func (r *Repository) options(ctx context.Context) ([]client.Option, error) {
	if r.Credentials == nil || !strings.HasPrefix(r.Remote, "https://") && !strings.HasPrefix(r.Remote, "http://") {
		return nil, nil
	}
	username, password, err := r.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	return []client.Option{client.WithHTTPAuth(&http.BasicAuth{Username: username, Password: password})}, nil
}

// DefaultBranch returns the branch origin's HEAD points at: the checkout's
// record of it, or the remote's answer when the checkout never recorded one.
func (r *Repository) DefaultBranch(ctx context.Context) (string, error) {
	var target plumbing.ReferenceName
	head, err := r.repo.Reference(plumbing.NewRemoteHEADReferenceName(remoteName), false)
	switch {
	case err == nil && head.Type() == plumbing.SymbolicReference:
		target = head.Target()
	case err == nil || errors.Is(err, plumbing.ErrReferenceNotFound):
		opts, err := r.options(ctx)
		if err != nil {
			return "", err
		}
		refs, err := r.origin.ListContext(ctx, &gogit.ListOptions{ClientOptions: opts})
		if err != nil {
			return "", fmt.Errorf("git: ls-remote: %w", err)
		}
		for _, ref := range refs {
			if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
				target = ref.Target()
			}
		}
	default:
		return "", fmt.Errorf("git: %w", err)
	}
	branch, ok := strings.CutPrefix(target.String(), "refs/remotes/origin/")
	if !ok {
		branch, ok = strings.CutPrefix(target.String(), "refs/heads/")
	}
	if !ok || branch == "" {
		return "", errors.New("git: origin has no default branch")
	}
	return branch, nil
}

// Fetch updates origin's remote-tracking refs for the given refspecs and
// prunes the ones they no longer match.
func (r *Repository) Fetch(ctx context.Context, refspecs ...string) error {
	opts, err := r.options(ctx)
	if err != nil {
		return err
	}
	specs := make([]config.RefSpec, len(refspecs))
	for i, refspec := range refspecs {
		specs[i] = config.RefSpec(refspec)
	}
	err = r.repo.FetchContext(ctx, &gogit.FetchOptions{RemoteName: remoteName, RefSpecs: specs, Prune: true, Tags: plumbing.NoTags, ClientOptions: opts})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return fmt.Errorf("git: fetch: %w", err)
	}
	return nil
}

// Tip returns the commit a fully qualified ref points at, or "" when the ref
// does not exist.
func (r *Repository) Tip(ref string) (string, error) {
	resolved, err := r.repo.Reference(plumbing.ReferenceName(ref), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("git: %s: %w", ref, err)
	}
	return resolved.Hash().String(), nil
}

// Branches lists origin's branches under a prefix.
func (r *Repository) Branches(prefix string) ([]string, error) {
	refs, err := r.repo.References()
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	var branches []string
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		branch, ok := strings.CutPrefix(ref.Name().String(), "refs/remotes/origin/")
		if ok && ref.Type() == plumbing.HashReference && strings.HasPrefix(branch, prefix) {
			branches = append(branches, branch)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	slices.Sort(branches)
	return branches, nil
}

// commit resolves a SHA or a fully qualified ref to its commit.
func (r *Repository) commit(ref string) (*object.Commit, error) {
	hash := plumbing.NewHash(ref)
	if !plumbing.IsHash(ref) {
		resolved, err := r.repo.Reference(plumbing.ReferenceName(ref), true)
		if err != nil {
			return nil, fmt.Errorf("git: %s: %w", ref, err)
		}
		hash = resolved.Hash()
	}
	commit, err := r.repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("git: %s: %w", ref, err)
	}
	return commit, nil
}

// Count reports how many commits tip has beyond base: zero, one, or two for
// any larger number, which is all ownership needs to tell apart.
func (r *Repository) Count(base, tip string) (int, error) {
	reviewed, err := r.commit(base)
	if err != nil {
		return 0, err
	}
	head, err := r.commit(tip)
	if err != nil {
		return 0, err
	}
	count := 0
	err = object.NewCommitPreorderIter(head, nil, nil).ForEach(func(c *object.Commit) error {
		contained, err := c.IsAncestor(reviewed)
		if err != nil {
			return err
		}
		if contained {
			return storer.ErrStop
		}
		count++
		if count == 2 {
			return storer.ErrStop
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("git: %w", err)
	}
	return count, nil
}

// Commit describes the commit a SHA or ref points at.
func (r *Repository) Commit(ref string) (Commit, error) {
	c, err := r.commit(ref)
	if err != nil {
		return Commit{}, err
	}
	commit := Commit{SHA: c.Hash.String(), AuthorEmail: c.Author.Email, CommitterEmail: c.Committer.Email, Trailers: trailers(c.Message)}
	if len(c.ParentHashes) > 0 {
		commit.Parent = c.ParentHashes[0].String()
	}
	return commit, nil
}

// trailers reads the key-value lines of a message's final paragraph, the
// block git treats as trailers.
func trailers(message string) map[string]string {
	result := map[string]string{}
	paragraphs := strings.Split(strings.TrimRight(message, "\n"), "\n\n")
	if len(paragraphs) < 2 {
		return result
	}
	for line := range strings.SplitSeq(paragraphs[len(paragraphs)-1], "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			result[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return result
}

// Show reads a path from a commit; a missing path returns nil.
func (r *Repository) Show(commit, path string) ([]byte, error) {
	c, err := r.commit(commit)
	if err != nil {
		return nil, err
	}
	file, err := c.File(path)
	if errors.Is(err, object.ErrFileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("git: %s: %w", path, err)
	}
	contents, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("git: %s: %w", path, err)
	}
	return []byte(contents), nil
}

// overlay is the storage behind a worktree: it reads the checkout's objects
// and keeps references, the index and every new object in memory, so the
// checkout never changes underneath its owner.
type overlay struct {
	*memory.Storage

	objects storer.EncodedObjectStorer
}

func (o *overlay) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if obj, err := o.Storage.EncodedObject(t, h); err == nil {
		return obj, nil
	}
	return o.objects.EncodedObject(t, h)
}

func (o *overlay) HasEncodedObject(h plumbing.Hash) error {
	if o.Storage.HasEncodedObject(h) == nil {
		return nil
	}
	return o.objects.HasEncodedObject(h)
}

func (o *overlay) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	if size, err := o.Storage.EncodedObjectSize(h); err == nil {
		return size, nil
	}
	return o.objects.EncodedObjectSize(h)
}

func (o *overlay) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	return o.objects.IterEncodedObjects(t)
}

var _ storage.Storer = (*overlay)(nil)

// Worktree is a detached checkout of one commit in a temporary directory. The
// commits it makes reach the checkout only through origin, on the next fetch.
// Callers remove it.
type Worktree struct {
	Dir string

	repo   *Repository
	local  *gogit.Repository
	commit plumbing.Hash
}

// Worktree checks a commit out into a new temporary directory.
func (r *Repository) Worktree(commit string) (*Worktree, error) {
	c, err := r.commit(commit)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "stemma-worktree-*")
	if err != nil {
		return nil, err
	}
	w := &Worktree{Dir: dir, repo: r, commit: c.Hash}
	if err := w.checkout(); err != nil {
		_ = w.Remove()
		return nil, fmt.Errorf("git: worktree: %w", err)
	}
	return w, nil
}

func (w *Worktree) checkout() error {
	store := memory.NewStorage()
	if err := store.SetReference(plumbing.NewHashReference(plumbing.HEAD, w.commit)); err != nil {
		return err
	}
	cfg := config.NewConfig()
	cfg.Remotes[remoteName] = w.repo.origin.Config()
	if err := store.SetConfig(cfg); err != nil {
		return err
	}
	local, err := gogit.Open(&overlay{Storage: store, objects: w.repo.repo.Storer}, osfs.New(w.Dir))
	if err != nil {
		return err
	}
	tree, err := local.Worktree()
	if err != nil {
		return err
	}
	if err := tree.Checkout(&gogit.CheckoutOptions{Hash: w.commit, Force: true}); err != nil {
		return err
	}
	w.local = local
	return nil
}

// Commit records the given paths on top of the worktree's commit and returns
// the new commit, which the worktree then has checked out.
func (w *Worktree) Commit(message string, paths ...string) (string, error) {
	tree, err := w.local.Worktree()
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}
	for _, path := range paths {
		if _, err := tree.Add(path); err != nil {
			return "", fmt.Errorf("git: add %s: %w", path, err)
		}
	}
	signature := &object.Signature{Name: w.repo.Identity.Name, Email: w.repo.Identity.Email, When: time.Now()}
	commit, err := tree.Commit(message, &gogit.CommitOptions{Author: signature, Committer: signature})
	if err != nil {
		return "", fmt.Errorf("git: commit: %w", err)
	}
	w.commit = commit
	return commit.String(), nil
}

// Push publishes the worktree's commit to an origin branch only while the
// branch still has the tip the caller observed; "" expects the branch to be
// absent.
func (w *Worktree) Push(ctx context.Context, branch, expected string) error {
	opts, err := w.repo.options(ctx)
	if err != nil {
		return err
	}
	// The lease compares origin's advertised tip with the worktree's record
	// of it, which is set to what the caller observed.
	observed := plumbing.ZeroHash
	if expected != "" {
		observed = plumbing.NewHash(expected)
	}
	local := plumbing.NewBranchReferenceName(branch)
	if err := w.local.Storer.SetReference(plumbing.NewHashReference(local, w.commit)); err != nil {
		return err
	}
	if err := w.local.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName(remoteName, branch), observed)); err != nil {
		return err
	}
	err = w.local.PushContext(ctx, &gogit.PushOptions{RemoteName: remoteName, RefSpecs: []config.RefSpec{config.RefSpec(local + ":" + local)}, ForceWithLease: &gogit.ForceWithLease{RefName: local}, ClientOptions: opts})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return fmt.Errorf("git: push %s: %w", branch, err)
	}
	return nil
}

// Remove deletes the worktree.
func (w *Worktree) Remove() error {
	return os.RemoveAll(w.Dir)
}

// DeleteBranch removes an origin branch while it still has the expected tip.
func (r *Repository) DeleteBranch(ctx context.Context, branch, expected string) error {
	opts, err := r.options(ctx)
	if err != nil {
		return err
	}
	name := plumbing.NewBranchReferenceName(branch)
	err = r.repo.PushContext(ctx, &gogit.PushOptions{RemoteName: remoteName, RefSpecs: []config.RefSpec{config.RefSpec(":" + name)}, RequireRemoteRefs: []config.RefSpec{config.RefSpec(expected + ":" + name.String())}, ClientOptions: opts})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return fmt.Errorf("git: delete %s: %w", branch, err)
	}
	return nil
}
