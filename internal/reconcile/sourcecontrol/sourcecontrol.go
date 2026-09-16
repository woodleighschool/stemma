// Package sourcecontrol defines what reconciliation needs from the host that
// reviews a repository: credentials for its git remote, pull requests and
// commit statuses. Providers implement it for one host each.
package sourcecontrol

import "context"

// Pull request states a provider lists.
const (
	Open   = "open"
	Closed = "closed"
)

// Commit status states.
const (
	Success = "success"
	Failure = "failure"
)

// PullRequest is one proposal as the host records it.
type PullRequest struct {
	Number      int
	Title, Body string
	URL         string
	// Head is the proposing branch and HeadSHA its tip when listed.
	Head, HeadSHA string
	Open, Merged  bool
}

// Status is one named commit status.
type Status struct {
	Name, State, Description string
}

// Identity is the commit author and committer the host attributes to the
// configured credentials.
type Identity struct {
	Name, Email string
}

// Provider is a source-control host holding the repository being reconciled.
type Provider interface {
	// Credentials returns the username and password git presents to the
	// origin remote.
	Credentials(ctx context.Context) (username, password string, err error)
	// Identity returns the commit identity the host recognises for the
	// credentials, so its own UI attributes the commits and so ownership of
	// a branch can be told from the commits on it.
	Identity(ctx context.Context) (Identity, error)
	// PullRequests lists pull requests in a state; a head branch narrows them.
	PullRequests(ctx context.Context, state, head string) ([]PullRequest, error)
	CreatePullRequest(ctx context.Context, head, base, title, body string) (PullRequest, error)
	UpdatePullRequest(ctx context.Context, number int, title, body string) error
	ClosePullRequest(ctx context.Context, number int) error
	// SetCommitStatus records a status on a commit. Descriptions are
	// summaries; the detail lives in the pull request or the run's logs.
	SetCommitStatus(ctx context.Context, sha string, status Status) error
	// CommitStatus returns the recorded state of one status name on a commit,
	// or "" when none was recorded.
	CommitStatus(ctx context.Context, sha, name string) (string, error)
}
