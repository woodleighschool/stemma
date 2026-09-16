// Package github is the source-control provider for GitHub and GitHub
// Enterprise Server, using the thin REST surface reconciliation needs.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/woodleighschool/stemma/internal/reconcile/sourcecontrol"
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
	api, owner, repository string
	client                 *http.Client
	// app is set for App authentication; token yields the static token
	// otherwise.
	app   *app
	token string

	mu       sync.Mutex
	identity *sourcecontrol.Identity
}

// app mints tokens for one installation from the App key and renews them
// before they expire, so a run longer than a token can still push.
type app struct {
	clientID     string
	installation int64
	key          *rsa.PrivateKey

	mu      sync.Mutex
	token   string
	expires time.Time
}

var _ sourcecontrol.Provider = (*Client)(nil)

// New binds the repository behind an origin URL to its host's API.
func New(remote string, settings map[string]any) (*Client, error) {
	cfg, err := decode(settings)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	api, owner, repository, err := parseRemote(remote)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	c := &Client{api: api, owner: owner, repository: repository, client: &http.Client{Timeout: time.Minute}, token: cfg.Token}
	if cfg.Token == "" {
		key, err := parseKey(cfg.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("github: %w", err)
		}
		installation, err := cfg.InstallationID.Int64()
		if err != nil || installation <= 0 {
			return nil, fmt.Errorf("github: config: installation_id %q is not a positive integer", cfg.InstallationID)
		}
		c.app = &app{clientID: cfg.ClientID, installation: installation, key: key}
	}
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

// parseRemote derives the API endpoint and repository from an origin URL in
// any form git accepts for GitHub: HTTPS, SSH or the scp-like shorthand.
func parseRemote(remote string) (api, owner, repository string, err error) {
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
	api = scheme + "://" + host + "/api/v3"
	if host == "github.com" {
		api = "https://api.github.com"
	}
	return api, owner, repository, nil
}

// parseKey accepts the PKCS#1 key GitHub issues and a PKCS#8 conversion of it.
func parseKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("private_key is not PEM encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private_key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private_key must be RSA")
	}
	return key, nil
}

// assertion signs a short-lived App JWT with the client ID as its issuer.
func (a *app) assertion() (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{Issuer: a.clientID, IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute))}
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(a.key)
	if err != nil {
		return "", fmt.Errorf("github: app assertion: %w", err)
	}
	return assertion, nil
}

// accessToken returns a token valid for the next request: the configured
// token, or a token minted for the App's installation.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	if c.app == nil {
		return c.token, nil
	}
	a := c.app
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > 5*time.Minute {
		return a.token, nil
	}
	assertion, err := a.assertion()
	if err != nil {
		return "", err
	}
	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.call(ctx, assertion, http.MethodPost, "/app/installations/"+strconv.FormatInt(a.installation, 10)+"/access_tokens", nil, &minted); err != nil {
		return "", err
	}
	a.token, a.expires = minted.Token, minted.ExpiresAt
	return a.token, nil
}

// Credentials presents the access token as the git password.
func (c *Client) Credentials(ctx context.Context) (string, string, error) {
	token, err := c.accessToken(ctx)
	return "x-access-token", token, err
}

// Identity derives the noreply identity GitHub attributes commits to: the
// App's bot user, or the token's user.
func (c *Client) Identity(ctx context.Context) (sourcecontrol.Identity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identity != nil {
		return *c.identity, nil
	}
	var login string
	if c.app != nil {
		assertion, err := c.app.assertion()
		if err != nil {
			return sourcecontrol.Identity{}, err
		}
		var app struct {
			Slug string `json:"slug"`
		}
		if err := c.call(ctx, assertion, http.MethodGet, "/app", nil, &app); err != nil {
			return sourcecontrol.Identity{}, err
		}
		login = app.Slug + "[bot]"
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	path := "/user"
	if login != "" {
		path = "/users/" + url.PathEscape(login)
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &user); err != nil {
		return sourcecontrol.Identity{}, err
	}
	if login == "" {
		login = user.Login
	}
	c.identity = &sourcecontrol.Identity{Name: login, Email: strconv.FormatInt(user.ID, 10) + "+" + login + "@users.noreply.github.com"}
	return *c.identity, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, result any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	return c.call(ctx, token, method, path, body, result)
}

func (c *Client) call(ctx context.Context, token, method, path string, body, result any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "stemma")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &failure)
		return fmt.Errorf("github %s %s: HTTP %d %s", method, path, res.StatusCode, failure.Message)
	}
	if result != nil && len(data) > 0 {
		if err := json.Unmarshal(data, result); err != nil {
			return fmt.Errorf("github %s %s: %w", method, path, err)
		}
	}
	return nil
}

type pullRequest struct {
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	State    string  `json:"state"`
	HTMLURL  string  `json:"html_url"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p pullRequest) record() sourcecontrol.PullRequest {
	return sourcecontrol.PullRequest{Number: p.Number, Title: p.Title, Body: p.Body, URL: p.HTMLURL, Head: p.Head.Ref, HeadSHA: p.Head.SHA, Open: p.State == sourcecontrol.Open, Merged: p.MergedAt != nil}
}

func (c *Client) repo(path string) string {
	return "/repos/" + c.owner + "/" + c.repository + path
}

// PullRequests lists pull requests in a state, page by page.
func (c *Client) PullRequests(ctx context.Context, state, head string) ([]sourcecontrol.PullRequest, error) {
	var pulls []sourcecontrol.PullRequest
	for page := 1; page <= 20; page++ {
		query := url.Values{"state": {state}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if head != "" {
			query.Set("head", c.owner+":"+head)
		}
		var batch []pullRequest
		if err := c.do(ctx, http.MethodGet, c.repo("/pulls?"+query.Encode()), nil, &batch); err != nil {
			return nil, err
		}
		for _, pull := range batch {
			pulls = append(pulls, pull.record())
		}
		if len(batch) < 100 {
			break
		}
	}
	return pulls, nil
}

func (c *Client) CreatePullRequest(ctx context.Context, head, base, title, body string) (sourcecontrol.PullRequest, error) {
	var pull pullRequest
	err := c.do(ctx, http.MethodPost, c.repo("/pulls"), map[string]any{"head": head, "base": base, "title": title, "body": body}, &pull)
	return pull.record(), err
}

func (c *Client) UpdatePullRequest(ctx context.Context, number int, title, body string) error {
	return c.do(ctx, http.MethodPatch, c.repo("/pulls/"+strconv.Itoa(number)), map[string]any{"title": title, "body": body}, nil)
}

func (c *Client) ClosePullRequest(ctx context.Context, number int) error {
	return c.do(ctx, http.MethodPatch, c.repo("/pulls/"+strconv.Itoa(number)), map[string]any{"state": "closed"}, nil)
}

// SetCommitStatus records a status, keeping the description within the 140
// characters GitHub stores.
func (c *Client) SetCommitStatus(ctx context.Context, sha string, status sourcecontrol.Status) error {
	description := status.Description
	if runes := []rune(description); len(runes) > 140 {
		description = string(runes[:139]) + "…"
	}
	return c.do(ctx, http.MethodPost, c.repo("/statuses/"+sha), map[string]any{"state": status.State, "context": status.Name, "description": description}, nil)
}

func (c *Client) CommitStatus(ctx context.Context, sha, name string) (string, error) {
	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := c.do(ctx, http.MethodGet, c.repo("/commits/"+sha+"/status"), nil, &combined); err != nil {
		return "", err
	}
	for _, status := range combined.Statuses {
		if status.Context == name {
			return status.State, nil
		}
	}
	return "", nil
}
