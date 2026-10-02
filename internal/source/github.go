package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/go-github/v92/github"
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
	Token              string `json:"token,omitempty" jsonschema_description:"Optional bearer token for release discovery and acquisition."`
}

type githubObservation struct {
	URL       string `json:"url"`
	Release   string `json:"release"`
	ReleaseID int64  `json:"release_id"`
	AssetID   int64  `json:"asset_id"`
	SHA256    string `json:"sha256,omitempty"`
	Filename  string `json:"filename"`
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
	var observed githubObservation
	if err := m.github(ctx, s, &observed); err != nil {
		return Discovery{}, err
	}
	data, err := json.Marshal(observed)
	if err != nil {
		return Discovery{}, err
	}
	request, err := acquireGitHub(ctx, s, data)
	if err != nil {
		return Discovery{}, err
	}
	found := Discovery{Observation: data, Immutable: true, Version: observed.Release}
	if request.Download.SHA256 != "" {
		found.Content = &Content{SHA256: request.Download.SHA256, Filename: request.Download.Filename, Mode: 0o644}
	}
	return found, nil
}

func acquireGitHub(_ context.Context, s githubConfig, observation json.RawMessage) (Acquisition, error) {
	var observed githubObservation
	if err := decode(observation, &observed); err != nil {
		return Acquisition{}, fmt.Errorf("GitHub observation: %w", err)
	}
	if err := validateHTTPURL(observed.URL); err != nil {
		return Acquisition{}, fmt.Errorf("GitHub observation URL: %w", err)
	}
	u, _ := url.Parse(observed.URL)
	if u.Scheme != "https" || u.Host != "github.com" || !strings.HasPrefix(u.Path, "/"+s.Repository+"/releases/download/") {
		return Acquisition{}, errors.New("observed asset does not belong to the configured GitHub repository")
	}
	if observed.ReleaseID <= 0 || observed.AssetID <= 0 || observed.Release == "" || !validFilename(observed.Filename) {
		return Acquisition{}, errors.New("GitHub observation requires a release tag, release and asset IDs, and asset filename")
	}
	if s.Release != "" && s.Release != "latest" && !doublestar.MatchUnvalidated(s.Release, observed.Release) {
		return Acquisition{}, errors.New("observed release does not match the configured GitHub release")
	}
	if !doublestar.MatchUnvalidated(s.Asset, observed.Filename) {
		return Acquisition{}, errors.New("observed asset does not match the configured GitHub asset")
	}
	if u.Path != "/"+s.Repository+"/releases/download/"+observed.Release+"/"+observed.Filename || path.Clean(u.Path) != u.Path || u.RawQuery != "" {
		return Acquisition{}, errors.New("observed GitHub URL does not identify the selected release asset")
	}
	digest := s.SHA256
	if observed.SHA256 != "" {
		if !validDigest(observed.SHA256) {
			return Acquisition{}, errors.New("GitHub observation contains an invalid sha256 digest")
		}
		if digest != "" && digest != observed.SHA256 {
			return Acquisition{}, errors.New("declared sha256 disagrees with registry digest")
		}
		digest = observed.SHA256
	}
	filename := s.Filename
	if filename == "" {
		filename = observed.Filename
	}
	var headers map[string]string
	if s.Token != "" {
		headers = map[string]string{"Authorization": "Bearer " + s.Token}
	}
	return Acquisition{Download: &Download{URL: observed.URL, Headers: headers, Filename: filename, SHA256: digest}}, nil
}

// github identifies one asset of one published release. The downloader
// transfers its bytes.
func (m *Manager) github(ctx context.Context, s githubConfig, observed *githubObservation) (err error) {
	done := plugin.Stage(ctx, "Discovering GitHub release", plugin.Detail(s.Repository))
	defer func() { done(err, plugin.Detail(observed.Release)) }()
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
	*observed = githubObservation{URL: asset.GetBrowserDownloadURL(), Release: release.TagName, ReleaseID: release.ID, AssetID: asset.GetID(), Filename: asset.GetName()}
	if digest, ok := strings.CutPrefix(asset.GetDigest(), "sha256:"); ok {
		if !validDigest(digest) {
			return errors.New("GitHub asset returned an invalid sha256 digest")
		}
		observed.SHA256 = digest
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
		return release, githubError(err)
	case !pattern && !s.IncludePrereleases:
		release, _, err := client.Repositories.GetLatestRelease(ctx, owner, repo)
		return release, githubError(err)
	}
	// Publication time is independent of tag version and commit creation time.
	var newest *github.RepositoryRelease
	page := &github.ListOptions{PerPage: 100}
	for {
		releases, response, err := client.Repositories.ListReleases(ctx, owner, repo, page)
		if err != nil {
			return nil, githubError(err)
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

// githubError reports an API failure by its status. Other failures are
// transport errors.
func githubError(err error) error {
	var response *github.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		return fmt.Errorf("GitHub release lookup returned HTTP %d", response.Response.StatusCode)
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
