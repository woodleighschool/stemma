// Package github is the source-control provider for GitHub and GitHub
// Enterprise Server.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/jferrl/go-githubauth/v2"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol"
	"golang.org/x/oauth2"
)

// Config is the provider configuration a Project declares under
// reconcile.source_control.config: a GitHub App installation, or a token.
type Config struct {
	// ClientID, InstallationID and PrivateKey identify a GitHub App and its
	// installation on the repository's owner; short-lived installation
	// tokens are minted from them as needed. The installation ID is a
	// number in YAML or a string when it comes from the environment.
	ClientID       string      `json:"client_id,omitempty"`
	InstallationID json.Number `json:"installation_id,omitempty"`
	PrivateKey     string      `json:"private_key,omitempty"`
	// Token is a personal access token used instead of an App.
	Token string `json:"token,omitempty"`
}

// Client is one repository on one GitHub host.
type Client struct {
	owner, repository string
	// server is a GitHub Enterprise Server origin, or empty for github.com.
	server string
	http   *http.Client
	// token yields the credential for the API and git: the configured token,
	// or an installation token renewed before it expires.
	token oauth2.TokenSource
	// app yields App assertions; it is nil for a token.
	app oauth2.TokenSource

	mu       sync.Mutex
	identity *sourcecontrol.Identity
}

var _ sourcecontrol.Provider = (*Client)(nil)

// New binds the repository behind an origin URL to its host's API. Tokens are
// minted within ctx.
func New(ctx context.Context, remote string, settings map[string]any) (*Client, error) {
	cfg, err := decode(settings)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	server, owner, repository, err := parseRemote(remote)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	c := &Client{owner: owner, repository: repository, server: server, http: &http.Client{Timeout: time.Minute}}
	if cfg.Token != "" {
		c.token = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Token})
		return c, nil
	}
	installation, err := cfg.InstallationID.Int64()
	if err != nil || installation <= 0 {
		return nil, fmt.Errorf("github: config: installation_id %q is not a positive integer", cfg.InstallationID)
	}
	c.app, err = githubauth.NewApplicationTokenSource(cfg.ClientID, []byte(cfg.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("github: private_key: %w", err)
	}
	// The auth transport attaches an App assertion to every request, including
	// redirects. Token minting must stay at the configured endpoint.
	authHTTP := *c.http
	authHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	options := []githubauth.InstallationTokenSourceOpt{githubauth.WithContext(ctx), githubauth.WithHTTPClient(&authHTTP)}
	if server != "" {
		options = append(options, githubauth.WithEnterpriseURL(server))
	}
	c.token = githubauth.NewInstallationTokenSource(installation, c.app, options...)
	return c, nil
}

func decode(settings map[string]any) (Config, error) {
	data, err := json.Marshal(settings)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	app := cfg.ClientID != "" || cfg.InstallationID != "" || cfg.PrivateKey != ""
	if (cfg.Token == "") == !app {
		return Config{}, errors.New("config: set either token or client_id, installation_id and private_key")
	}
	if app && (cfg.ClientID == "" || cfg.InstallationID == "" || cfg.PrivateKey == "") {
		return Config{}, errors.New("config: a GitHub App needs client_id, installation_id and private_key")
	}
	return cfg, nil
}

// parseRemote derives the host and repository from an origin URL in any form
// git accepts for GitHub: HTTPS, SSH or the scp-like shorthand. The server is
// empty for github.com and the host's origin otherwise.
func parseRemote(remote string) (server, owner, repository string, err error) {
	var host, path string
	scheme := "https"
	if u, parseErr := url.Parse(remote); parseErr == nil && u.Host != "" {
		host, path = u.Host, u.Path
		if u.Scheme == "http" {
			scheme = "http"
		}
	} else {
		_, rest, _ := strings.Cut(remote, "@")
		host, path, _ = strings.Cut(rest, ":")
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, repository, ok := strings.Cut(path, "/")
	if host == "" || !ok || owner == "" || repository == "" || strings.Contains(repository, "/") {
		return "", "", "", fmt.Errorf("origin %q is not a GitHub repository URL", remote)
	}
	if host != "github.com" {
		server = scheme + "://" + host
	}
	return server, owner, repository, nil
}

// rest returns an API client that sends the current credential from source.
// go-github attaches only a fixed token, and only to its own origins, so each
// call binds the token it is sent with.
func (c *Client) rest(source oauth2.TokenSource) (*github.Client, error) {
	token, err := source.Token()
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	options := []github.ClientOptionsFunc{github.WithHTTPClient(c.http), github.WithUserAgent("stemma"), github.WithAuthToken(token.AccessToken)}
	if c.server != "" {
		options = append(options, github.WithEnterpriseURLs(c.server, c.server))
	}
	return github.NewClient(options...)
}

// Credentials presents the access token as the git password.
func (c *Client) Credentials(context.Context) (string, string, error) {
	token, err := c.token.Token()
	if err != nil {
		return "", "", fmt.Errorf("github: %w", err)
	}
	return "x-access-token", token.AccessToken, nil
}

// Identity derives the noreply identity GitHub attributes commits to: the
// App's bot user, or the token's user.
func (c *Client) Identity(ctx context.Context) (sourcecontrol.Identity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identity != nil {
		return *c.identity, nil
	}
	// An empty login reads the token's own user.
	var login string
	if c.app != nil {
		api, err := c.rest(c.app)
		if err != nil {
			return sourcecontrol.Identity{}, err
		}
		app, _, err := api.Apps.Get(ctx, "")
		if err != nil {
			return sourcecontrol.Identity{}, fmt.Errorf("github: app: %w", err)
		}
		login = app.GetSlug() + "[bot]"
	}
	api, err := c.rest(c.token)
	if err != nil {
		return sourcecontrol.Identity{}, err
	}
	user, _, err := api.Users.Get(ctx, login)
	if err != nil {
		return sourcecontrol.Identity{}, fmt.Errorf("github: user: %w", err)
	}
	c.identity = &sourcecontrol.Identity{Name: user.GetLogin(), Email: fmt.Sprintf("%d+%s@users.noreply.github.com", user.GetID(), user.GetLogin())}
	return *c.identity, nil
}

func record(pull *github.PullRequest) sourcecontrol.PullRequest {
	return sourcecontrol.PullRequest{
		Number: pull.GetNumber(), Title: pull.GetTitle(), Body: pull.GetBody(), URL: pull.GetHTMLURL(),
		Head: pull.GetHead().GetRef(), HeadSHA: pull.GetHead().GetSHA(),
		Open: pull.GetState() == sourcecontrol.Open, Merged: pull.MergedAt != nil,
	}
}

// PullRequests lists pull requests in a state, page by page.
func (c *Client) PullRequests(ctx context.Context, state, head string) ([]sourcecontrol.PullRequest, error) {
	api, err := c.rest(c.token)
	if err != nil {
		return nil, err
	}
	options := &github.PullRequestListOptions{State: state, PerPage: 100}
	if head != "" {
		options.Head = c.owner + ":" + head
	}
	var pulls []sourcecontrol.PullRequest
	for {
		batch, response, err := api.PullRequests.List(ctx, c.owner, c.repository, options)
		if err != nil {
			return nil, fmt.Errorf("github: pull requests: %w", err)
		}
		for _, pull := range batch {
			pulls = append(pulls, record(pull))
		}
		if response.NextPage == 0 {
			return pulls, nil
		}
		options.Page = response.NextPage
	}
}

func (c *Client) CreatePullRequest(ctx context.Context, head, base, title, body string) (sourcecontrol.PullRequest, error) {
	api, err := c.rest(c.token)
	if err != nil {
		return sourcecontrol.PullRequest{}, err
	}
	pull, _, err := api.PullRequests.Create(ctx, c.owner, c.repository, github.CreatePullRequest{Head: head, Base: base, Title: &title, Body: &body})
	if err != nil {
		return sourcecontrol.PullRequest{}, fmt.Errorf("github: create pull request: %w", err)
	}
	return record(pull), nil
}

func (c *Client) UpdatePullRequest(ctx context.Context, number int, title, body string) error {
	return c.edit(ctx, number, &github.PullRequest{Title: &title, Body: &body})
}

func (c *Client) ClosePullRequest(ctx context.Context, number int) error {
	return c.edit(ctx, number, &github.PullRequest{State: new("closed")})
}

func (c *Client) edit(ctx context.Context, number int, pull *github.PullRequest) error {
	api, err := c.rest(c.token)
	if err != nil {
		return err
	}
	if _, _, err := api.PullRequests.Edit(ctx, c.owner, c.repository, number, pull); err != nil {
		return fmt.Errorf("github: pull request %d: %w", number, err)
	}
	return nil
}

// SetCommitStatus records a status, keeping the description within the 140
// characters GitHub stores.
func (c *Client) SetCommitStatus(ctx context.Context, sha string, status sourcecontrol.Status) error {
	api, err := c.rest(c.token)
	if err != nil {
		return err
	}
	description := status.Description
	if runes := []rune(description); len(runes) > 140 {
		description = string(runes[:139]) + "…"
	}
	if _, _, err := api.Repositories.CreateStatus(ctx, c.owner, c.repository, sha, github.RepoStatus{State: &status.State, Context: &status.Name, Description: &description}); err != nil {
		return fmt.Errorf("github: commit status: %w", err)
	}
	return nil
}

func (c *Client) CommitStatus(ctx context.Context, sha, name string) (string, error) {
	api, err := c.rest(c.token)
	if err != nil {
		return "", err
	}
	options := &github.ListOptions{PerPage: 100}
	for {
		combined, response, err := api.Repositories.GetCombinedStatus(ctx, c.owner, c.repository, sha, options)
		if err != nil {
			return "", fmt.Errorf("github: commit status: %w", err)
		}
		for _, status := range combined.Statuses {
			if status.GetContext() == name {
				return status.GetState(), nil
			}
		}
		if response.NextPage == 0 {
			return "", nil
		}
		options.Page = response.NextPage
	}
}
