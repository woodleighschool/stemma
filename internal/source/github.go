package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/go-github/v92/github"
	"github.com/woodleighschool/stemma/plugin"
)

// github identifies one asset of one published release. The downloader
// transfers its bytes.
func (m *Manager) github(ctx context.Context, s nativeConfig, observed *nativeObservation) (err error) {
	done := plugin.Stage(ctx, "Discovering GitHub release", plugin.Detail(s.Repository))
	defer func() { done(err, plugin.Detail(observed.Release)) }()
	options := []github.ClientOptionsFunc{github.WithHTTPClient(m.Client), github.WithUserAgent(userAgent)}
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
	*observed = nativeObservation{URL: asset.GetBrowserDownloadURL(), Release: release.TagName, ReleaseID: release.ID, AssetID: asset.GetID()}
	if digest, ok := strings.CutPrefix(asset.GetDigest(), "sha256:"); ok {
		if !validDigest(digest) {
			return errors.New("GitHub asset returned an invalid sha256 digest")
		}
		observed.SHA256, observed.Filename = digest, asset.GetName()
	}
	return nil
}

// selectRelease finds the release a declaration names: an exact tag, GitHub's
// latest release, or the newest published release among those that match a tag
// glob or include prereleases.
func selectRelease(ctx context.Context, client *github.Client, s nativeConfig) (*github.RepositoryRelease, error) {
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
