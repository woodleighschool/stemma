package source

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/go-github/v93/github"
	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

type githubConfig struct {
	Repository         string `json:"repository" jsonschema_description:"GitHub repository in owner/name form."`
	Release            string `json:"release,omitempty" jsonschema_description:"GitHub release tag, tag glob, or latest. Omitted or empty values select latest. A value containing *, ? or [ selects the newest published release whose tag matches, using doublestar semantics."`
	IncludePrereleases bool   `json:"include_prereleases,omitempty" jsonschema_description:"Include prereleases when discovering latest or a tag glob; select the newest published non-draft release. Explicit release tags are unchanged."`
	Asset              string `json:"asset" jsonschema_description:"GitHub asset-name glob using doublestar semantics. Must match exactly one release asset; exact names also work."`
	Filename           string `json:"filename,omitempty" jsonschema_description:"Optional input basename override. Defaults to the selected asset name. Independent of publication naming."`
	SHA256             string `json:"sha256,omitempty" jsonschema_description:"Optional expected SHA-256 content digest, as 64 lowercase hexadecimal characters."`
	Token              string `json:"token,omitempty" jsonschema_description:"Optional bearer token for release discovery and acquisition. Without one, discovery uses GH_TOKEN, then GITHUB_TOKEN, then reconcile's GitHub App when its origin is github.com. Fallback tokens are used only for API discovery."`
}

// githubOrigin locates one asset of one published release. The download URL
// carries the release tag and asset name.
type githubOrigin struct {
	URL       string `json:"url"`
	ReleaseID int64  `json:"release_id"`
	AssetID   int64  `json:"asset_id"`
}

// githubAsset is the asset discovery selected, with the digest GitHub reports
// for it, when it reports one.
type githubAsset struct {
	githubOrigin

	Release  string
	Filename string
	SHA256   string
}

func (githubConfig) JSONSchemaExtend(schema *jsonschema.Schema) {
	token, _ := schema.Properties.Get("token")
	token.WriteOnly = true
}

func githubInput(input plugin.Input) (githubConfig, error) {
	s, err := configFor[githubConfig](input)
	if err != nil {
		return s, err
	}
	owner, repo, ok := strings.Cut(s.Repository, "/")
	if !ok || owner == "" || repo == "" || strings.ContainsAny(owner+repo, "/ ?#\\") || strings.IndexFunc(s.Repository, unicode.IsControl) >= 0 || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return s, errors.New("GitHub source requires repository owner/name")
	}
	if s.Asset == "" || !doublestar.ValidatePattern(s.Asset) {
		return s, fmt.Errorf("invalid GitHub asset pattern %q", s.Asset)
	}
	if releasePattern(s.Release) && !doublestar.ValidatePattern(s.Release) {
		return s, fmt.Errorf("invalid GitHub release pattern %q", s.Release)
	}
	if s.Release == "" {
		s.Release = "latest"
	}
	if s.Token != "" {
		if err := validateHeaders(map[string]string{"Authorization": "Bearer " + s.Token}); err != nil {
			return s, err
		}
	}
	return s, validateContent(s.Filename, s.SHA256)
}

func (s githubConfig) sourceIdentity() any {
	s.Token = ""
	return s
}

func (m *Manager) githubResolver() Resolver {
	return resolverFor(githubInput, m.discoverGitHub, acquireGitHub)
}

func (m *Manager) discoverGitHub(ctx context.Context, s githubConfig) (Discovery, error) {
	var asset githubAsset
	if err := m.github(ctx, s, &asset); err != nil {
		return Discovery{}, err
	}
	origin, err := json.Marshal(asset.githubOrigin)
	if err != nil {
		return Discovery{}, err
	}
	digest := s.SHA256
	if asset.SHA256 != "" {
		if digest != "" && digest != asset.SHA256 {
			return Discovery{}, errors.New("declared sha256 disagrees with registry digest")
		}
		digest = asset.SHA256
	}
	found := Discovery{Origin: origin, Immutable: true, Version: asset.Release}
	if digest != "" {
		found.Content = &Content{SHA256: digest, Filename: cmp.Or(s.Filename, asset.Filename), Mode: 0o644}
	}
	release, name, err := s.locate(asset.githubOrigin)
	if err != nil {
		return Discovery{}, err
	}
	if release != asset.Release || name != asset.Filename {
		return Discovery{}, errors.New("GitHub URL does not identify the selected release asset")
	}
	if err := s.selects(release, name); err != nil {
		return Discovery{}, err
	}
	return found, nil
}

func acquireGitHub(_ context.Context, s githubConfig, entry Entry) (Acquisition, error) {
	var located githubOrigin
	if err := decode(entry.Origin, &located); err != nil {
		return Acquisition{}, fmt.Errorf("GitHub origin: %w", err)
	}
	release, name, err := s.locate(located)
	if err != nil {
		return Acquisition{}, err
	}
	if err := s.selects(release, name); err != nil {
		return Acquisition{}, err
	}
	digest := s.SHA256
	if expected := entry.expected(); expected != nil {
		if digest != "" && digest != expected.SHA256 {
			return Acquisition{}, errors.New("declared sha256 disagrees with the locked content")
		}
		digest = expected.SHA256
	}
	var headers map[string]string
	if s.Token != "" {
		headers = map[string]string{"Authorization": "Bearer " + s.Token}
	}
	return Acquisition{Download: &Download{URL: located.URL, Headers: headers, Filename: cmp.Or(s.Filename, name), SHA256: digest}}, nil
}

// locate reads the release tag and asset name from an origin's download URL,
// which must belong to the configured repository.
func (s githubConfig) locate(located githubOrigin) (release, name string, err error) {
	if err := validateHTTPURL(located.URL); err != nil {
		return "", "", fmt.Errorf("GitHub origin URL: %w", err)
	}
	u, _ := url.Parse(located.URL)
	rest, ok := strings.CutPrefix(u.Path, "/"+s.Repository+"/releases/download/")
	if u.Scheme != "https" || u.Host != "github.com" || !ok || path.Clean(u.Path) != u.Path || u.RawQuery != "" {
		return "", "", errors.New("GitHub origin does not identify a release asset of the configured repository")
	}
	slash := strings.LastIndex(rest, "/")
	if slash <= 0 || located.ReleaseID <= 0 || located.AssetID <= 0 || !validFilename(rest[slash+1:]) {
		return "", "", errors.New("GitHub origin requires a release asset URL and release and asset IDs")
	}
	return rest[:slash], rest[slash+1:], nil
}

// selects reports whether the declaration selects a release and asset.
func (s githubConfig) selects(release, name string) error {
	if s.Release != "" && s.Release != "latest" && !doublestar.MatchUnvalidated(s.Release, release) {
		return errors.New("origin release does not match the configured GitHub release")
	}
	if !doublestar.MatchUnvalidated(s.Asset, name) {
		return errors.New("origin asset does not match the configured GitHub asset")
	}
	return nil
}

// github identifies one asset of one published release. The downloader
// transfers its bytes.
func (m *Manager) github(ctx context.Context, s githubConfig, selected *githubAsset) (err error) {
	done := plugin.Stage(ctx, "Discovering GitHub release", plugin.Detail(s.Repository))
	defer func() { done(err, plugin.Detail(selected.Release)) }()
	if s.Token == "" {
		s.Token = m.GitHubToken
	}
	if s.Token == "" && m.GitHubAppToken != nil {
		token, err := m.GitHubAppToken.Token()
		if err != nil {
			return fmt.Errorf("GitHub release authentication: %w", err)
		}
		if token == nil || token.AccessToken == "" {
			return errors.New("GitHub release authentication: App returned no access token")
		}
		s.Token = token.AccessToken
	}
	options := []github.ClientOptionsFunc{github.WithHTTPClient(m.metadataClient()), github.WithUserAgent(userAgent)}
	if s.Token != "" {
		options = append(options, github.WithAuthToken(s.Token))
	}
	client, err := github.NewClient(options...)
	if err != nil {
		return err
	}
	release, err := selectRelease(ctx, client, s)
	if err != nil {
		return err
	}
	if release.Draft {
		return errors.New("draft releases are not supported")
	}
	var matches []*github.ReleaseAsset
	var names []string
	for _, asset := range release.Assets {
		if doublestar.MatchUnvalidated(s.Asset, asset.GetName()) {
			matches = append(matches, asset)
			names = append(names, asset.GetName())
		}
	}
	if len(matches) != 1 {
		if len(matches) == 0 {
			for _, asset := range release.Assets {
				names = append(names, asset.GetName())
			}
			return fmt.Errorf("GitHub release %s has no asset matching %q%s", release.TagName, s.Asset, assetNames("available assets", names))
		}
		return fmt.Errorf("GitHub release %s has %d assets matching %q; expected exactly one%s", release.TagName, len(matches), s.Asset, assetNames("matched assets", names))
	}
	asset := matches[0]
	*selected = githubAsset{URL: asset.GetBrowserDownloadURL(), ReleaseID: release.ID, AssetID: asset.GetID(), Release: release.TagName, Filename: asset.GetName()}
	if digest, ok := strings.CutPrefix(asset.GetDigest(), "sha256:"); ok {
		if !validDigest(digest) {
			return errors.New("GitHub asset returned an invalid sha256 digest")
		}
		selected.SHA256 = digest
	}
	return nil
}

// selectRelease finds the release a declaration names: an exact tag, GitHub's
// latest release, or the newest published release among those that match a tag
// glob or include prereleases.
func selectRelease(ctx context.Context, client *github.Client, s githubConfig) (*github.RepositoryRelease, error) {
	owner, repo, _ := strings.Cut(s.Repository, "/")
	pattern := releasePattern(s.Release)
	latest := s.Release == "" || s.Release == "latest"
	switch {
	case !pattern && !latest:
		release, _, err := client.Repositories.GetReleaseByTag(ctx, owner, repo, url.PathEscape(s.Release))
		return release, githubError(err, s.Token == "")
	case !pattern && !s.IncludePrereleases:
		release, _, err := client.Repositories.GetLatestRelease(ctx, owner, repo)
		return release, githubError(err, s.Token == "")
	}
	// Publication time is independent of tag version and commit creation time.
	var newest *github.RepositoryRelease
	page := &github.ListOptions{PerPage: 100}
	for {
		releases, response, err := client.Repositories.ListReleases(ctx, owner, repo, page)
		if err != nil {
			return nil, githubError(err, s.Token == "")
		}
		for _, candidate := range releases {
			if candidate.Draft || candidate.Prerelease && !s.IncludePrereleases || pattern && !doublestar.MatchUnvalidated(s.Release, candidate.TagName) {
				continue
			}
			if candidate.PublishedAt == nil {
				return nil, errors.New("published GitHub release is missing published_at")
			}
			if newest == nil {
				newest = candidate
				continue
			}
			published, current := candidate.PublishedAt.Time, newest.PublishedAt.Time
			if published.After(current) || published.Equal(current) && candidate.ID > newest.ID {
				newest = candidate
			}
		}
		if response.NextPage == 0 {
			break
		}
		page.Page = response.NextPage
	}
	switch {
	case newest != nil:
		return newest, nil
	case pattern:
		return nil, fmt.Errorf("no published GitHub release tag matches %q", s.Release)
	}
	return nil, errors.New("no published GitHub releases found")
}

// githubError reports an API failure by its status and GitHub's reason. Other
// failures are transport errors.
func githubError(err error, anonymous bool) error {
	var api *github.ErrorResponse
	var primary *github.RateLimitError
	var secondary *github.AbuseRateLimitError
	var response *http.Response
	var message string
	switch {
	case errors.As(err, &api):
		response, message = api.Response, api.Message
	case errors.As(err, &primary):
		response, message = primary.Response, primary.Message
	case errors.As(err, &secondary):
		response, message = secondary.Response, secondary.Message
	}
	if response != nil {
		status := response.StatusCode
		failure := fmt.Sprintf("GitHub release lookup returned HTTP %d", status)
		if message != "" {
			failure += ": " + message
		}
		// GitHub answers an exhausted anonymous quota with either status.
		if anonymous && (status == http.StatusForbidden || status == http.StatusTooManyRequests) {
			failure += "; set GH_TOKEN or GITHUB_TOKEN to authenticate lookups"
		}
		return errors.New(failure)
	}
	if err != nil {
		return transportError("GitHub release lookup", err)
	}
	return nil
}

// releasePattern reports whether a release value is a tag glob. Git forbids
// these characters in tag names, so no exact tag reads as a glob.
func releasePattern(release string) bool {
	return strings.ContainsAny(release, "*?[")
}

func assetNames(label string, names []string) string {
	if len(names) == 0 {
		return "; " + label + ": none"
	}
	if len(names) > 10 {
		return fmt.Sprintf("; %s: %d total", label, len(names))
	}
	return fmt.Sprintf("; %s: %q", label, names)
}
