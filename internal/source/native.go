package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

type nativeConfig struct {
	Type       string   `json:"-"`
	Include    []string `json:"include,omitempty"`
	Base       string   `json:"base,omitempty"`
	URL        string   `json:"url,omitempty"`
	Match      string   `json:"match,omitempty"`
	Path       string   `json:"path,omitempty"`
	Repository string   `json:"repository,omitempty"`
	Release    string   `json:"release,omitempty"`
	Asset      string   `json:"asset,omitempty"`
	Filename   string   `json:"filename,omitempty"`
	SHA256     string   `json:"sha256,omitempty"`
	Token      string   `json:"token,omitempty"`
}

type nativeObservation struct {
	URL       string `json:"url,omitempty"`
	Release   string `json:"release,omitempty"`
	ReleaseID int64  `json:"release_id,omitempty"`
	AssetID   int64  `json:"asset_id,omitempty"`
}

type nativeEntry struct {
	nativeObservation
	Filename string
	Tree     bool
}

func native(input plugin.Input) (nativeConfig, error) {
	var s nativeConfig
	data, err := json.Marshal(input.Config)
	if err != nil {
		return s, err
	}
	if err := decode(data, &s); err != nil {
		return s, fmt.Errorf("%s resolver config: %w", input.Resolver, err)
	}
	s.Type = input.Resolver
	if s.Type == "file" && s.Path != "" {
		s.Path, err = relativeTo(input.Base, s.Path)
	} else if s.Type == "local" {
		s.Base, err = relativeTo(input.Base, s.Base)
	}
	if err != nil {
		return s, err
	}
	return s, s.Validate()
}

func relativeTo(base, name string) (string, error) {
	if path.IsAbs(name) || path.IsAbs(base) || strings.ContainsAny(base+name, "\\:\x00\r\n") {
		return "", errors.New("local input path must be relative to its resource file")
	}
	resolved := path.Join(base, name)
	if resolved == "" {
		resolved = "."
	}
	if !safeRelative(resolved) {
		return "", errors.New("local input path must stay inside the project")
	}
	return resolved, nil
}

func (m *Manager) resolveNative(ctx context.Context, input plugin.Input) (Content, json.RawMessage, error) {
	s, err := native(input)
	if err != nil {
		return Content{}, nil, err
	}
	entry := nativeEntry{URL: s.URL, Filename: s.Filename, Tree: s.Type == "local"}
	mode := uint32(0o644)
	if s.Type == "file" || s.Type == "local" {
		root, err := os.OpenRoot(m.Root)
		if err != nil {
			return Content{}, nil, err
		}
		defer func() { _ = root.Close() }()
		name := s.Path
		if s.Type == "local" {
			name = s.Base
			if name == "" {
				name = "."
			}
		}
		info, err := root.Lstat(name)
		if err != nil {
			return Content{}, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return Content{}, nil, errors.New("input root must not be a symlink")
		}
		entry.Tree = info.IsDir()
		mode = uint32(info.Mode().Perm())
	}
	if s.Type == "github" {
		if err := m.github(ctx, s, &entry); err != nil {
			return Content{}, nil, err
		}
	}
	if s.Type == "http" && s.Match != "" {
		if err := m.discover(ctx, s, &entry); err != nil {
			return Content{}, nil, err
		}
	}
	if entry.Filename == "" {
		switch s.Type {
		case "local":
			entry.Filename = path.Base(s.Base)
			if entry.Filename == "." || entry.Filename == "" {
				entry.Filename = "local"
			}
		case "file":
			entry.Filename = filepath.Base(s.Path)
		default:
			u, err := url.Parse(entry.URL)
			if err != nil {
				return Content{}, nil, err
			}
			entry.Filename = path.Base(u.Path)
		}
	}
	if !validFilename(entry.Filename) {
		return Content{}, nil, errors.New("input has no safe filename; set filename explicitly")
	}
	ref, err := m.download(ctx, s, entry, s.SHA256)
	observation, encodeErr := json.Marshal(entry.nativeObservation)
	if err != nil {
		return Content{}, nil, err
	}
	return Content{Artifact: ref, Filename: entry.Filename, Tree: entry.Tree, Mode: mode}, observation, encodeErr
}

func (m *Manager) fetchNative(ctx context.Context, input plugin.Input, entry Entry) (Content, error) {
	s, err := native(input)
	if err != nil {
		return Content{}, err
	}
	var observation nativeObservation
	if err := decode(entry.Observation, &observation); err != nil {
		return Content{}, err
	}
	ref, err := m.download(ctx, s, nativeEntry{nativeObservation: observation, Filename: entry.Content.Filename, Tree: entry.Content.Tree}, entry.Content.Artifact.SHA256)
	content := entry.Content
	content.Artifact = ref
	return content, err
}

func safeRelative(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\:\x00\r\n") || !filepath.IsLocal(filepath.FromSlash(name)) {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func (s nativeConfig) Validate() error {
	if s.Filename != "" && (filepath.Base(s.Filename) != s.Filename || strings.ContainsAny(s.Filename, `/\\`) || s.Filename == "." || s.Filename == "..") {
		return errors.New("filename must be a basename")
	}
	if s.SHA256 != "" && !validDigest(s.SHA256) {
		return errors.New("sha256 must be 64 lowercase hexadecimal characters")
	}
	if s.Type != "local" && (len(s.Include) > 0 || s.Base != "") {
		return errors.New("include is only supported for local sources")
	}
	if s.Match != "" {
		if s.Type != "http" {
			return errors.New("match is only supported for HTTP sources")
		}
		if _, err := regexp.Compile(s.Match); err != nil {
			return fmt.Errorf("source match: %w", err)
		}
	}
	switch s.Type {
	case "http":
		if err := validateHTTPURL(s.URL); err != nil {
			return err
		}
		if s.Path != "" || s.Repository != "" || s.Release != "" || s.Asset != "" {
			return errors.New("HTTP source contains fields for another provider")
		}
	case "github":
		if len(strings.Split(s.Repository, "/")) != 2 || strings.ContainsAny(s.Repository, " ?#\\") || s.Asset == "" || s.URL != "" || s.Path != "" {
			return errors.New("GitHub source requires repository owner/name and an exact asset name")
		}
	case "file":
		if !safeRelative(s.Path) || s.URL != "" || s.Repository != "" || s.Release != "" || s.Asset != "" || s.Token != "" {
			return errors.New("file source requires a project-relative path only")
		}
	case "local":
		if len(s.Include) == 0 || s.Path != "" || s.URL != "" || s.Repository != "" || s.Release != "" || s.Asset != "" || s.Token != "" || (s.Base != "" && !safeRelative(s.Base)) {
			return errors.New("local source requires include patterns relative to its software-family file")
		}
		for _, pattern := range s.Include {
			if !safeRelative(pattern) || !doublestar.ValidatePattern(pattern) {
				return fmt.Errorf("invalid local include pattern %q", pattern)
			}
		}
	default:
		return fmt.Errorf("unsupported source type %q", s.Type)
	}
	return nil
}

// validateHTTPURL allows stable query identifiers, but excludes embedded
// credentials and commonly signed, expiring download references from locks.
func validateHTTPURL(address string) error {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("HTTP source requires an http(s) URL without credentials or fragment")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("HTTP source contains an invalid query")
	}
	for key := range query {
		key = strings.ToLower(key)
		if strings.HasPrefix(key, "x-amz-") || strings.HasPrefix(key, "x-goog-") {
			return errors.New("HTTP source must use a stable URL, not an expiring signed download")
		}
		switch strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", "") {
		case "token", "accesstoken", "authtoken", "auth", "authorization", "apikey", "key", "signature", "sig", "expires", "expiry", "expiration", "credential", "credentials", "password", "secret":
			return errors.New("HTTP source query must not contain credentials or expiration")
		}
	}
	return nil
}

func (m *Manager) download(ctx context.Context, s nativeConfig, entry nativeEntry, expected string) (result cas.Ref, err error) {
	if s.Type == "local" {
		done := plugin.Stage(ctx, "Reading local inputs")
		defer func() { done(err) }()
		project, err := os.OpenRoot(m.Root)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = project.Close() }()
		base := s.Base
		if base == "" {
			base = "."
		}
		root, err := project.OpenRoot(base)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = root.Close() }()
		var names []string
		seen := map[string]bool{}
		for _, pattern := range s.Include {
			matched := false
			err := doublestar.GlobWalk(root.FS(), pattern, func(name string, _ fs.DirEntry) error {
				matched = true
				if !seen[name] {
					if len(names) >= 100000 {
						return errors.New("local source exceeds 100000 entries")
					}
					seen[name] = true
					names = append(names, name)
				}
				return nil
			}, doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())
			if err != nil {
				return cas.Ref{}, fmt.Errorf("local include %q: %w", pattern, err)
			}
			if !matched {
				return cas.Ref{}, fmt.Errorf("local include %q matched no files", pattern)
			}
		}
		return m.importTree(ctx, root, names, expected)
	}
	if s.Type == "file" {
		done := plugin.Stage(ctx, "Reading local input")
		defer func() { done(err) }()
		root, err := os.OpenRoot(m.Root)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = root.Close() }()
		f, err := root.Open(s.Path)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return cas.Ref{}, err
		}
		if !info.Mode().IsRegular() {
			if !info.IsDir() || !entry.Tree {
				return cas.Ref{}, errors.New("file source changed type or is not a regular file/directory")
			}
			tree, err := root.OpenRoot(s.Path)
			if err != nil {
				return cas.Ref{}, err
			}
			defer func() { _ = tree.Close() }()
			return m.importTree(ctx, tree, nil, expected)
		}
		if entry.Tree {
			return cas.Ref{}, errors.New("file source changed from directory to file")
		}
		if err := archive.CheckMetadata(f, info); err != nil {
			return cas.Ref{}, err
		}
		return m.Store.Import(ctx, f, expected)
	}
	if err := validateHTTPURL(entry.URL); err != nil {
		return cas.Ref{}, fmt.Errorf("lockfile URL: %w", err)
	}
	u, _ := url.Parse(entry.URL)
	token := s.Token
	if s.Type == "http" {
		if s.Match == "" && entry.URL != s.URL {
			return cas.Ref{}, errors.New("locked HTTP URL does not match configuration")
		}
		if s.Match != "" {
			pattern, err := regexp.Compile(s.Match)
			if err != nil || pattern.FindString(entry.URL) != entry.URL {
				return cas.Ref{}, errors.New("locked HTTP URL does not match source pattern")
			}
		}
		origin, _ := url.Parse(s.URL)
		if origin.Scheme == "https" && u.Scheme != "https" {
			return cas.Ref{}, errors.New("refusing discovered HTTPS downgrade")
		}
		if origin.Scheme != u.Scheme || origin.Host != u.Host {
			token = ""
		}
	}
	if s.Type == "github" && (u.Host != "github.com" || !strings.HasPrefix(u.Path, "/"+s.Repository+"/releases/download/")) {
		return cas.Ref{}, errors.New("locked asset does not belong to the configured GitHub repository")
	}
	done := plugin.Stage(ctx, "Downloading input")
	defer func() { done(err) }()
	req, err := m.request(ctx, entry.URL, token)
	if err != nil {
		return cas.Ref{}, err
	}
	res, err := m.Client.Do(req)
	if err != nil {
		return cas.Ref{}, transportError("download", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return cas.Ref{}, fmt.Errorf("download returned HTTP %d", res.StatusCode)
	}
	if res.ContentLength > cas.MaxObjectSize {
		return cas.Ref{}, errors.New("download exceeds 16 GiB")
	}
	plugin.Logger(ctx).DebugContext(ctx, "Download response", "bytes", res.ContentLength)
	return m.Store.Import(ctx, plugin.ProgressReader(ctx, res.Body, res.ContentLength), expected)
}

func (m *Manager) importTree(ctx context.Context, root *os.Root, names []string, expected string) (cas.Ref, error) {
	staging, err := os.CreateTemp(filepath.Join(m.Store.Dir, "work"), "source-*.tar")
	if err != nil {
		return cas.Ref{}, err
	}
	defer func() { _ = os.Remove(staging.Name()) }()
	packErr := archive.PackSelected(ctx, root, names, staging)
	closeErr := staging.Close()
	if packErr != nil {
		return cas.Ref{}, packErr
	}
	if closeErr != nil {
		return cas.Ref{}, closeErr
	}
	return m.Store.ImportFile(ctx, staging.Name(), expected)
}

func (m *Manager) request(ctx context.Context, address, token string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "stemma/0.1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func (m *Manager) discover(ctx context.Context, s nativeConfig, entry *nativeEntry) (err error) {
	done := plugin.Stage(ctx, "Discovering source release")
	defer func() { done(err) }()
	req, err := m.request(ctx, s.URL, s.Token)
	if err != nil {
		return err
	}
	res, err := m.Client.Do(req)
	if err != nil {
		return transportError("download page", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download page returned HTTP %d", res.StatusCode)
	}
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return transportError("download page", err)
	}
	if len(data) > limit {
		return errors.New("download page exceeds 4 MiB")
	}
	pattern, err := regexp.Compile(s.Match)
	if err != nil {
		return err
	}
	matches := map[string]bool{}
	for _, address := range pattern.FindAllString(html.UnescapeString(string(data)), -1) {
		if err := validateHTTPURL(address); err != nil {
			return fmt.Errorf("download page match must be a complete stable URL: %w", err)
		}
		matches[address] = true
	}
	if len(matches) != 1 {
		return fmt.Errorf("download page matched %d distinct artifact URLs; expected one", len(matches))
	}
	for address := range matches {
		entry.URL = address
	}
	return nil
}

func (m *Manager) github(ctx context.Context, s nativeConfig, entry *nativeEntry) (err error) {
	done := plugin.Stage(ctx, "Discovering GitHub release")
	defer func() { done(err) }()
	endpoint := "https://api.github.com/repos/" + s.Repository + "/releases/latest"
	if s.Release != "" && s.Release != "latest" {
		endpoint = "https://api.github.com/repos/" + s.Repository + "/releases/tags/" + url.PathEscape(s.Release)
	}
	req, err := m.request(ctx, endpoint, s.Token)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := m.Client.Do(req)
	if err != nil {
		return transportError("GitHub release lookup", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub release lookup returned HTTP %d", res.StatusCode)
	}
	var release struct {
		ID     int64  `json:"id"`
		Tag    string `json:"tag_name"`
		Draft  bool   `json:"draft"`
		Assets []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&release); err != nil {
		return err
	}
	if release.Draft {
		return errors.New("draft releases are not supported")
	}
	for _, asset := range release.Assets {
		if asset.Name == s.Asset {
			entry.URL = asset.URL
			entry.ReleaseID = release.ID
			entry.AssetID = asset.ID
			if entry.Filename == "" {
				entry.Filename = asset.Name
			}
			entry.Release = release.Tag
			return nil
		}
	}
	return fmt.Errorf("GitHub release %s has no asset %q", release.Tag, s.Asset)
}

func validFilename(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00\r\n") && filepath.IsLocal(name)
}

// Redirect targets may contain temporary credentials. Do not include their URLs
// in reports or durable state when a request fails.
func transportError(operation string, err error) error {
	var requestError *url.Error
	for errors.As(err, &requestError) {
		err = requestError.Err
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}
