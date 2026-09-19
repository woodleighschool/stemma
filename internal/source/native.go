package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/net/http/httpguts"
)

type nativeConfig struct {
	Type               string            `json:"-"`
	Include            []string          `json:"include,omitempty" jsonschema_description:"Local tree globs using doublestar semantics. Each pattern must match at least one entry."`
	Base               string            `json:"base,omitempty" jsonschema_description:"Local tree root, relative to the resource file. Defaults to its directory."`
	URL                string            `json:"url,omitempty" jsonschema_description:"Stable HTTP download URL. Redirects are followed without retaining temporary URLs in the lockfile."`
	Match              string            `json:"match,omitempty" jsonschema_description:"HTTP page regular expression whose full matches are URL references resolved against the final page URL. Must identify one distinct stable HTTP(S) download URL."`
	Path               string            `json:"path,omitempty" jsonschema_description:"Exact file or directory path relative to the resource file, confined to the project."`
	Repository         string            `json:"repository,omitempty" jsonschema_description:"GitHub repository in owner/name form."`
	Release            string            `json:"release,omitempty" jsonschema_description:"GitHub release tag, or latest. Omitted or empty values select latest."`
	IncludePrereleases bool              `json:"include_prereleases,omitempty" jsonschema_description:"Include prereleases when discovering latest; select the newest published non-draft release. Explicit release tags are unchanged."`
	Asset              string            `json:"asset,omitempty" jsonschema_description:"GitHub asset-name glob using doublestar semantics. Must match exactly one release asset; exact names also work."`
	Filename           string            `json:"filename,omitempty" jsonschema_description:"Optional input basename override. HTTP defaults to Content-Disposition, then final and original URL basenames; GitHub uses the selected asset name; file and local use the selected path or base. Independent of publication naming."`
	SHA256             string            `json:"sha256,omitempty" jsonschema_description:"Optional expected SHA-256 content digest, as 64 lowercase hexadecimal characters."`
	Token              string            `json:"token,omitempty" jsonschema_description:"Optional bearer token. Mutually exclusive with an Authorization header."`
	Headers            map[string]string `json:"headers,omitempty" jsonschema_description:"HTTP request headers, including optional User-Agent and Referer overrides. Credentials and custom headers are confined to the source origin."`
}

// Native field sets also constrain the editor schema.
var nativeFields = map[string][]string{
	"http":   {"url", "match", "filename", "sha256", "token", "headers"},
	"github": {"repository", "release", "include_prereleases", "asset", "filename", "sha256", "token"},
	"file":   {"path", "filename", "sha256"},
	"local":  {"base", "include", "filename", "sha256"},
}

// nativeObservation records where locked content came from. ETag and
// Last-Modified are refresh hints: they let a later refresh ask the server
// whether the same bytes still stand, and never identify content themselves.
type nativeObservation struct {
	URL          string `json:"url,omitempty"`
	Release      string `json:"release,omitempty"`
	ReleaseID    int64  `json:"release_id,omitempty"`
	AssetID      int64  `json:"asset_id,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

func (o nativeObservation) hints() bool { return o.ETag != "" || o.LastModified != "" }

type nativeEntry struct {
	nativeObservation

	Filename string
	Tree     bool
	// revalidate carries the previous hints a refresh may send conditionally.
	revalidate nativeObservation
}

// errNotModified reports that a conditional download confirmed the previous content.
var errNotModified = errors.New("source content not modified")

// NativeResolver reports whether the name is reserved for a built-in source.
func NativeResolver(name string) bool {
	_, ok := nativeFields[name]
	return ok
}

// ValidateInput checks a native declaration without acquiring its content.
func ValidateInput(input plugin.Input) error {
	_, err := native(input)
	return err
}

func native(input plugin.Input) (nativeConfig, error) {
	var s nativeConfig
	for field := range input.Config {
		if !slices.Contains(nativeFields[input.Resolver], field) {
			return s, fmt.Errorf("%s resolver does not support field %q", input.Resolver, field)
		}
	}
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
	if err := s.Validate(); err != nil {
		return s, err
	}
	if len(s.Headers) > 0 {
		headers := make(map[string]string, len(s.Headers))
		for name, value := range s.Headers {
			headers[http.CanonicalHeaderKey(name)] = value
		}
		s.Headers = headers
	}
	return s, nil
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
	content, observation, _, err := m.observeNative(ctx, input, nil)
	return content, observation, err
}

// refreshNative asks the source whether previous still stands before
// downloading: GitHub identifies assets by release and asset ID, HTTP by a
// conditional request with the recorded hints. Unchanged bytes keep the
// previous hints so a rotated validator alone never changes a lock; entries
// locked without hints adopt them once.
func (m *Manager) refreshNative(ctx context.Context, input plugin.Input, previous Entry) (Content, json.RawMessage, bool, error) {
	var observed nativeObservation
	if err := decode(previous.Observation, &observed); err != nil {
		return Content{}, nil, false, fmt.Errorf("locked observation: %w", err)
	}
	content, observation, unchanged, err := m.observeNative(ctx, input, &observed)
	if err != nil || unchanged {
		return content, observation, unchanged, err
	}
	if content.Artifact == previous.Content.Artifact && observed.hints() {
		var current nativeObservation
		if err := decode(observation, &current); err != nil {
			return Content{}, nil, false, err
		}
		current.ETag, current.LastModified = observed.ETag, observed.LastModified
		observation, err = json.Marshal(current)
	}
	return content, observation, false, err
}

// observeNative resolves a declaration, or with a previous observation of the
// same declaration reports unchanged when the source confirms its content.
func (m *Manager) observeNative(ctx context.Context, input plugin.Input, previous *nativeObservation) (Content, json.RawMessage, bool, error) {
	s, err := native(input)
	if err != nil {
		return Content{}, nil, false, err
	}
	entry := nativeEntry{URL: s.URL, Filename: s.Filename, Tree: s.Type == "local"}
	mode := uint32(0o644)
	if s.Type == "file" || s.Type == "local" {
		root, err := os.OpenRoot(m.Root)
		if err != nil {
			return Content{}, nil, false, err
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
			return Content{}, nil, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return Content{}, nil, false, errors.New("input root must not be a symlink")
		}
		entry.Tree = info.IsDir()
		mode = uint32(info.Mode().Perm())
	}
	if s.Type == "github" {
		if err := m.github(ctx, s, &entry); err != nil {
			return Content{}, nil, false, err
		}
		if previous != nil && entry.nativeObservation == *previous {
			return Content{}, nil, true, nil
		}
	}
	if s.Type == "http" && s.Match != "" {
		if err := m.discover(ctx, s, &entry); err != nil {
			return Content{}, nil, false, err
		}
	}
	if s.Type == "http" && previous != nil && previous.URL == entry.URL {
		entry.revalidate = *previous
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
		}
	}
	if s.Type != "http" && !validFilename(entry.Filename) {
		return Content{}, nil, false, errors.New("input has no safe filename; set filename explicitly")
	}
	ref, err := m.download(ctx, s, &entry, s.SHA256)
	if errors.Is(err, errNotModified) {
		return Content{}, nil, true, nil
	}
	observation, encodeErr := json.Marshal(entry.nativeObservation)
	if err != nil {
		return Content{}, nil, false, err
	}
	return Content{Artifact: ref, Filename: entry.Filename, Tree: entry.Tree, Mode: mode}, observation, false, encodeErr
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
	download := nativeEntry{nativeObservation: observation, Filename: entry.Content.Filename, Tree: entry.Content.Tree}
	ref, err := m.download(ctx, s, &download, entry.Content.Artifact.SHA256)
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
	if s.Filename != "" && !validFilename(s.Filename) {
		return errors.New("filename must be a basename")
	}
	seen := map[string]bool{}
	for name, value := range s.Headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return errors.New("invalid HTTP header name or value")
		}
		name = http.CanonicalHeaderKey(name)
		if seen[name] {
			return errors.New("HTTP header names must be unique ignoring case")
		}
		seen[name] = true
		switch name {
		case "Host", "Content-Length", "Transfer-Encoding", "Trailer":
			return fmt.Errorf("HTTP header %s is managed by the downloader", name)
		case "Authorization":
			if s.Token != "" {
				return errors.New("use token or an Authorization header, not both")
			}
		}
	}
	if s.SHA256 != "" && !validDigest(s.SHA256) {
		return errors.New("sha256 must be 64 lowercase hexadecimal characters")
	}
	if s.Match != "" {
		if _, err := regexp.Compile(s.Match); err != nil {
			return fmt.Errorf("source match: %w", err)
		}
	}
	switch s.Type {
	case "http":
		if err := validateHTTPURL(s.URL); err != nil {
			return err
		}
	case "github":
		owner, repo, ok := strings.Cut(s.Repository, "/")
		if !ok || owner == "" || repo == "" || strings.ContainsAny(owner+repo, "/ ?#\\") || strings.IndexFunc(s.Repository, unicode.IsControl) >= 0 || owner == "." || owner == ".." || repo == "." || repo == ".." {
			return errors.New("GitHub source requires repository owner/name")
		}
		if s.Asset == "" || !doublestar.ValidatePattern(s.Asset) {
			return fmt.Errorf("invalid GitHub asset pattern %q", s.Asset)
		}
	case "file":
		if !safeRelative(s.Path) {
			return errors.New("file source requires a project-relative path only")
		}
	case "local":
		if len(s.Include) == 0 || (s.Base != "" && !safeRelative(s.Base)) {
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

func (m *Manager) download(ctx context.Context, s nativeConfig, entry *nativeEntry, expected string) (result cas.Ref, err error) {
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
		return m.Store.Import(ctx, f, expected)
	}
	if err := validateHTTPURL(entry.URL); err != nil {
		return cas.Ref{}, fmt.Errorf("lockfile URL: %w", err)
	}
	u, _ := url.Parse(entry.URL)
	if s.Type == "http" {
		if s.Match == "" && entry.URL != s.URL {
			return cas.Ref{}, errors.New("locked HTTP URL does not match configuration")
		}
	}
	if s.Type == "github" && (u.Host != "github.com" || !strings.HasPrefix(u.Path, "/"+s.Repository+"/releases/download/")) {
		return cas.Ref{}, errors.New("locked asset does not belong to the configured GitHub repository")
	}
	done := plugin.Stage(ctx, "Downloading input")
	defer func() {
		if errors.Is(err, errNotModified) {
			done(nil, "unchanged", true)
			return
		}
		done(err)
	}()
	req, err := m.request(ctx, entry.URL, s)
	if err != nil {
		return cas.Ref{}, err
	}
	if entry.revalidate.ETag != "" {
		req.Header.Set("If-None-Match", entry.revalidate.ETag)
	}
	if entry.revalidate.LastModified != "" {
		req.Header.Set("If-Modified-Since", entry.revalidate.LastModified)
	}
	res, err := m.Client.Do(req)
	if err != nil {
		return cas.Ref{}, transportError("download", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotModified && entry.revalidate.hints() {
		return cas.Ref{}, errNotModified
	}
	if res.StatusCode != http.StatusOK {
		return cas.Ref{}, fmt.Errorf("download returned HTTP %d", res.StatusCode)
	}
	// Hosts that ignore conditional requests still send validators; the same
	// strong ETag confirms the locked bytes without transferring them.
	if s.Type == "http" && entry.revalidate.ETag != "" && !strings.HasPrefix(entry.revalidate.ETag, "W/") && res.Header.Get("ETag") == entry.revalidate.ETag && res.Header.Get("Last-Modified") == entry.revalidate.LastModified {
		return cas.Ref{}, errNotModified
	}
	if res.ContentLength > cas.MaxObjectSize {
		return cas.Ref{}, errors.New("download exceeds 16 GiB")
	}
	if s.Type == "http" {
		entry.ETag, entry.LastModified = res.Header.Get("ETag"), res.Header.Get("Last-Modified")
	}
	if entry.Filename == "" {
		entry.Filename = responseFilename(res, entry.URL)
	}
	if !validFilename(entry.Filename) {
		return cas.Ref{}, errors.New("input has no safe filename; set filename explicitly")
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

func (m *Manager) request(ctx context.Context, address string, s nativeConfig) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "stemma/0.1")
	for name, value := range s.Headers {
		req.Header.Set(name, value)
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	if s.Type == "http" {
		origin, _ := url.Parse(s.URL)
		if !sameOrigin(origin, req.URL) {
			stripPrivateHeaders(req.Header)
		}
	}
	return req, nil
}

func responseFilename(response *http.Response, original string) string {
	if _, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition")); err == nil && validFilename(params["filename"]) {
		return params["filename"]
	}
	if response.Request != nil {
		if name := path.Base(response.Request.URL.Path); validFilename(name) {
			return name
		}
	}
	address, _ := url.Parse(original)
	if name := path.Base(address.Path); validFilename(name) {
		return name
	}
	return ""
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

func stripPrivateHeaders(headers http.Header) {
	for name := range headers {
		switch http.CanonicalHeaderKey(name) {
		case "Accept", "Accept-Encoding", "Accept-Language", "User-Agent", "If-None-Match", "If-Modified-Since":
		default:
			headers.Del(name)
		}
	}
}

func (m *Manager) discover(ctx context.Context, s nativeConfig, entry *nativeEntry) (err error) {
	done := plugin.Stage(ctx, "Discovering source release")
	defer func() { done(err) }()
	req, err := m.request(ctx, s.URL, s)
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
	base := req.URL
	if res.Request != nil {
		base = res.Request.URL
	}
	matches := map[string]bool{}
	for _, address := range pattern.FindAllString(html.UnescapeString(string(data)), -1) {
		reference, err := url.Parse(address)
		if err != nil || address == "" {
			return errors.New("download page match is not a valid URL reference")
		}
		resolved := base.ResolveReference(reference)
		address = resolved.String()
		if err := validateHTTPURL(address); err != nil {
			return fmt.Errorf("download page match must resolve to a stable HTTP(S) URL: %w", err)
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
	var release githubRelease
	if s.IncludePrereleases && (s.Release == "" || s.Release == "latest") {
		// Publication time is independent of tag version and commit creation time.
		for page := 1; ; page++ {
			var releases []githubRelease
			endpoint := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100&page=%d", s.Repository, page)
			if err := m.githubResponse(ctx, s, endpoint, &releases); err != nil {
				return err
			}
			for _, candidate := range releases {
				if candidate.Draft {
					continue
				}
				if candidate.PublishedAt.IsZero() {
					return errors.New("published GitHub release is missing published_at")
				}
				if candidate.PublishedAt.After(release.PublishedAt) || candidate.PublishedAt.Equal(release.PublishedAt) && candidate.ID > release.ID {
					release = candidate
				}
			}
			if len(releases) < 100 {
				break
			}
		}
		if release.ID == 0 {
			return errors.New("no published GitHub releases found")
		}
	} else if err := m.githubResponse(ctx, s, endpoint, &release); err != nil {
		return err
	}
	if release.Draft {
		return errors.New("draft releases are not supported")
	}
	var matches []int
	var names []string
	for i, asset := range release.Assets {
		if doublestar.MatchUnvalidated(s.Asset, asset.Name) {
			matches = append(matches, i)
			names = append(names, asset.Name)
		}
	}
	if len(matches) != 1 {
		if len(matches) == 0 {
			for _, asset := range release.Assets {
				names = append(names, asset.Name)
			}
			return fmt.Errorf("GitHub release %s has no asset matching %q%s", release.Tag, s.Asset, assetNames("available assets", names))
		}
		return fmt.Errorf("GitHub release %s has %d assets matching %q; expected exactly one%s", release.Tag, len(matches), s.Asset, assetNames("matched assets", names))
	}
	asset := release.Assets[matches[0]]
	entry.URL = asset.URL
	entry.ReleaseID = release.ID
	entry.AssetID = asset.ID
	entry.Release = release.Tag
	if entry.Filename == "" {
		entry.Filename = asset.Name
	}
	return nil
}

type githubRelease struct {
	ID          int64     `json:"id"`
	Tag         string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Assets      []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (m *Manager) githubResponse(ctx context.Context, s nativeConfig, endpoint string, target any) error {
	req, err := m.request(ctx, endpoint, s)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	res, err := m.Client.Do(req)
	if err != nil {
		return transportError("GitHub release lookup", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub release lookup returned HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(target)
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

func validFilename(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:") && strings.IndexFunc(name, unicode.IsControl) < 0 && filepath.IsLocal(name)
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
